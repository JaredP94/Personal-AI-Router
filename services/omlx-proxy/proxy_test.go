// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"nvpair-shared/prefixhash"
)

// TestNodeURL covers the URL-construction part of nodeURL — specifically
// that IPv6 literals are bracket-wrapped and IPv4/hostnames remain
// byte-identical to the pre-JoinHostPort implementation. The
// local-address shortcut is not exercised here because it depends on
// the host's network interfaces (see init() in proxy.go) and would be
// flaky across environments.
func TestNodeURL(t *testing.T) {
	// Pick addresses that are unlikely to appear on any local interface.
	tests := []struct {
		name     string
		node     Node
		wantHost string
		wantURL  string
	}{
		{
			name:     "ipv4",
			node:     Node{Addresses: []string{"192.0.2.10"}, Port: 11434},
			wantHost: "192.0.2.10:11434",
			wantURL:  "http://192.0.2.10:11434",
		},
		{
			name:     "ipv6",
			node:     Node{Addresses: []string{"2001:db8::1"}, Port: 11434},
			wantHost: "[2001:db8::1]:11434",
			wantURL:  "http://[2001:db8::1]:11434",
		},
		{
			name:     "hostname",
			node:     Node{Addresses: []string{"gpu-host.lan"}, Port: 11434},
			wantHost: "gpu-host.lan:11434",
			wantURL:  "http://gpu-host.lan:11434",
		},
		{
			// Empty Addresses slice — nodeURL should fall back to Host.
			name:     "fallback to Host",
			node:     Node{Host: "gpu-host.lan", Port: 11434},
			wantHost: "gpu-host.lan:11434",
			wantURL:  "http://gpu-host.lan:11434",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u := nodeURL(tc.node)
			if u == nil {
				t.Fatalf("nodeURL returned nil")
			}
			if u.Host != tc.wantHost {
				t.Errorf("Host = %q, want %q", u.Host, tc.wantHost)
			}
			if got := u.String(); got != tc.wantURL {
				t.Errorf("String() = %q, want %q", got, tc.wantURL)
			}
		})
	}
}

// TestWithUsageRequestedAddsIncludeUsage: a streaming inference body gains
// stream_options.include_usage, which is the flag oMLX needs before it emits
// the usage block carrying the engine's prefill/decode numbers.
func TestWithUsageRequestedAddsIncludeUsage(t *testing.T) {
	body := []byte(`{"model":"m","messages":[],"stream":true}`)
	out := withUsageRequested(body)
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	var so map[string]json.RawMessage
	if err := json.Unmarshal(got["stream_options"], &so); err != nil {
		t.Fatal(err)
	}
	if string(so["include_usage"]) != "true" {
		t.Fatalf("include_usage: %s", so["include_usage"])
	}
}

// TestWithUsageRequestedKeepsExistingStreamOptions: the client's own
// stream_options are its choice — overriding them would change the semantics
// of a field the client set deliberately.
func TestWithUsageRequestedKeepsExistingStreamOptions(t *testing.T) {
	body := []byte(`{"model":"m","stream":true,"stream_options":{"include_usage":false}}`)
	out := withUsageRequested(body)
	if !strings.Contains(string(out), `"include_usage":false`) {
		t.Fatalf("client's stream_options must not be overridden: %s", out)
	}
}

// TestWithUsageRequestedLeavesNonStreamingAlone: oMLX emits no usage block on
// the non-streaming path, so a non-streaming body is forwarded untouched.
func TestWithUsageRequestedLeavesNonStreamingAlone(t *testing.T) {
	body := []byte(`{"model":"m","stream":false}`)
	out := withUsageRequested(body)
	if strings.Contains(string(out), "stream_options") {
		t.Fatalf("non-streaming body changed: %s", out)
	}
}

// TestWithUsageRequestedLeavesNonJSONAlone: a body that is not JSON is not a
// request oMLX would answer with a usage block; it is forwarded byte for byte.
func TestWithUsageRequestedLeavesNonJSONAlone(t *testing.T) {
	body := []byte("not json")
	if string(withUsageRequested(body)) != "not json" {
		t.Fatal("non-JSON body must be untouched")
	}
}

// TestWithUsageRequestedLeavesNonObjectAlone: a JSON body that is not an
// object (an array, a string, null) is not a request body oMLX would answer
// with a usage block; it is forwarded byte for byte.
func TestWithUsageRequestedLeavesNonObjectAlone(t *testing.T) {
	for _, body := range []string{`["a"]`, `"a"`, `null`, `123`, `true`} {
		if got := string(withUsageRequested([]byte(body))); got != body {
			t.Errorf("body %q changed to %q, want it untouched", body, got)
		}
	}
}

// TestWithUsageRequestedKeepsOtherFields: adding the flag must not change
// any other field of the request.
func TestWithUsageRequestedKeepsOtherFields(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true,"temperature":0.5,"max_tokens":16}`)
	out := withUsageRequested(body)
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"model", "messages", "stream", "temperature", "max_tokens"} {
		original := map[string]json.RawMessage{}
		if err := json.Unmarshal(body, &original); err != nil {
			t.Fatal(err)
		}
		if string(got[key]) != string(original[key]) {
			t.Errorf("field %q = %s, want %s", key, got[key], original[key])
		}
	}
}

// TestWithUsageRequestedDoesNotModifyInput: the caller's bytes are the
// client's request and must survive the call untouched — the flag belongs in
// a copy, never in the client's own body.
func TestWithUsageRequestedDoesNotModifyInput(t *testing.T) {
	body := []byte(`{"model":"m","stream":true}`)
	original := append([]byte(nil), body...)
	out := withUsageRequested(body)
	if !bytes.Equal(body, original) {
		t.Fatalf("the caller's body was modified: %s", body)
	}
	// The result must not alias the input either: writing to it must leave
	// the caller's bytes alone.
	if len(out) > 0 {
		out[0] = 'X'
		if !bytes.Equal(body, original) {
			t.Fatalf("the result aliases the caller's body: %s", body)
		}
	}
}

// TestUsageRequestKeepsPrefixHashDigest: the forwarded body is re-marshalled
// (its top-level keys are sorted, its whitespace is dropped), so the
// prefix-cache key must still be the client's key. prefixhash ignores
// stream_options and canonicalises the message fields it hashes, so the
// digest is unchanged — this pins that behaviour.
func TestUsageRequestKeepsPrefixHashDigest(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}],"stream":true,"temperature":0.2}`)
	injected := withUsageRequested(body)
	if bytes.Equal(injected, body) {
		t.Fatal("the usage request was not added, so this proves nothing")
	}
	before := prefixhash.Extract(http.MethodPost, "/v1/chat/completions", body)
	after := prefixhash.Extract(http.MethodPost, "/v1/chat/completions", injected)
	if before.Model != after.Model {
		t.Fatalf("model = %q after the injection, want %q", after.Model, before.Model)
	}
	if before.Full != after.Full {
		t.Fatalf("full digest = %x after the injection, want %x", after.Full, before.Full)
	}
	if len(before.Prefixes) != len(after.Prefixes) {
		t.Fatalf("prefix count = %d after the injection, want %d", len(after.Prefixes), len(before.Prefixes))
	}
	for i := range before.Prefixes {
		if before.Prefixes[i] != after.Prefixes[i] {
			t.Fatalf("prefix %d = %x after the injection, want %x", i, after.Prefixes[i], before.Prefixes[i])
		}
	}
}

// TestHandleHTTP_ForwardsUsageRequest: the engine receives the streaming body
// with the usage block requested — this is the whole point of the flag.
func TestHandleHTTP_ForwardsUsageRequest(t *testing.T) {
	var gotBody string
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: {\"choices\":[]}\n\n")
	}))
	defer engine.Close()

	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "engine", engine.URL, "llama"))
	p := testProxy(disc, 11434)

	rec := httptest.NewRecorder()
	p.handleHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"llama","stream":true,"messages":[]}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(gotBody), &got); err != nil {
		t.Fatalf("engine body %q is not JSON: %v", gotBody, err)
	}
	var so map[string]json.RawMessage
	if err := json.Unmarshal(got["stream_options"], &so); err != nil {
		t.Fatalf("engine body %q has no stream_options: %v", gotBody, err)
	}
	if string(so["include_usage"]) != "true" {
		t.Errorf("engine got include_usage = %s, want true", so["include_usage"])
	}
	if string(got["model"]) != `"llama"` {
		t.Errorf("engine got model = %s, want \"llama\"", got["model"])
	}
}

// TestHandleHTTP_ForwardsUsageRequestSpacedBody: a body whose whitespace
// makes it exactly as long as the injected body must still be injected. The
// caller cannot tell "unchanged" from "changed" by length alone, so the
// forwarded body is compared by content.
func TestHandleHTTP_ForwardsUsageRequestSpacedBody(t *testing.T) {
	// 71 bytes: the compacted body plus the 40 bytes the injection adds, so
	// the client's body and the injected body are the same length.
	spaced := `{"model":                                        "llama","stream":true}`
	if len(spaced) != 71 {
		t.Fatalf("spaced body is %d bytes, want 71", len(spaced))
	}
	var gotBody string
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"done":true}`)
	}))
	defer engine.Close()

	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "engine", engine.URL, "llama"))
	p := testProxy(disc, 11434)

	rec := httptest.NewRecorder()
	p.handleHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(spaced)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(gotBody, `"include_usage":true`) {
		t.Errorf("engine got body %q, want the usage request", gotBody)
	}
}

// TestHandleHTTP_NonStreamingBodyUnchanged: a non-streaming inference request
// is forwarded byte for byte.
func TestHandleHTTP_NonStreamingBodyUnchanged(t *testing.T) {
	var gotBody string
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"done":true}`)
	}))
	defer engine.Close()

	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "engine", engine.URL, "llama"))
	p := testProxy(disc, 11434)

	rec := httptest.NewRecorder()
	p.handleHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"llama","stream":false}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(gotBody, `"model":"llama"`) {
		t.Fatalf("engine got body %q, want the forwarded request", gotBody)
	}
	if strings.Contains(gotBody, "stream_options") {
		t.Errorf("engine got body %q, want it unchanged", gotBody)
	}
}

// TestHandleHTTP_ClientStreamOptionsPreserved: a client that asked for
// something specific in stream_options keeps exactly that.
func TestHandleHTTP_ClientStreamOptionsPreserved(t *testing.T) {
	var gotBody string
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"done":true}`)
	}))
	defer engine.Close()

	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "engine", engine.URL, "llama"))
	p := testProxy(disc, 11434)

	rec := httptest.NewRecorder()
	p.handleHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"llama","stream":true,"stream_options":{"include_usage":false}}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(gotBody, `"include_usage":false`) {
		t.Errorf("engine got body %q, want the client's stream_options", gotBody)
	}
}

// TestHandleHTTP_NonInferenceBodyUnchanged: a control endpoint is not an
// inference request, so its body is forwarded unchanged even when it happens
// to look like a streaming one.
func TestHandleHTTP_NonInferenceBodyUnchanged(t *testing.T) {
	var gotBody string
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"done":true}`)
	}))
	defer engine.Close()

	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "engine", engine.URL, "llama"))
	p := testProxy(disc, 11434)

	rec := httptest.NewRecorder()
	p.handleHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/something-else",
		strings.NewReader(`{"model":"llama","stream":true}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(gotBody, `"model":"llama"`) {
		t.Fatalf("engine got body %q, want the forwarded request", gotBody)
	}
	if strings.Contains(gotBody, "stream_options") {
		t.Errorf("engine got body %q, want it unchanged", gotBody)
	}
}

// TestHandleHTTP_ClientBodyNotModifiedInPlace: the bytes the client sent
// are the client's own; the usage request belongs in the forwarded copy.
func TestHandleHTTP_ClientBodyNotModifiedInPlace(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"done":true}`)
	}))
	defer engine.Close()

	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "engine", engine.URL, "llama"))
	p := testProxy(disc, 11434)

	raw := []byte(`{"model":"llama","stream":true,"messages":[]}`)
	original := append([]byte(nil), raw...)
	rec := httptest.NewRecorder()
	p.handleHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(raw)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !bytes.Equal(raw, original) {
		t.Fatalf("the client's body was modified in place: %s", raw)
	}
}

// TestHandleHTTP_ForwardedContentLengthMatchesBody: the forwarded body is
// longer than the client's, so the declared length has to follow it — an
// upstream request whose Content-Length disagrees with its body is
// malformed, and the engine would never see the usage request.
func TestHandleHTTP_ForwardedContentLengthMatchesBody(t *testing.T) {
	var (
		mu             sync.Mutex
		gotLength      int64
		gotBody        string
		gotBodyLength  int
		gotContentType string
	)
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotLength = r.ContentLength
		gotBody = string(b)
		gotBodyLength = len(b)
		gotContentType = r.Header.Get("Content-Type")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"done":true}`)
	}))
	defer engine.Close()

	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "engine", engine.URL, "llama"))
	p := testProxy(disc, 11434)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"llama","stream":true,"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	p.handleHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotLength != int64(gotBodyLength) {
		t.Errorf("engine saw Content-Length %d for a %d byte body", gotLength, gotBodyLength)
	}
	if !strings.Contains(gotBody, `"include_usage":true`) {
		t.Errorf("engine got body %q, want the usage request", gotBody)
	}
	if gotContentType != "application/json" {
		t.Errorf("engine saw Content-Type %q, want application/json", gotContentType)
	}
}

// TestHandleHTTP_ChunkedRequestForwardsWholeBody: a client that streams its
// request (no declared length) must still get the whole body forwarded, with
// the usage request in it.
func TestHandleHTTP_ChunkedRequestForwardsWholeBody(t *testing.T) {
	var (
		mu          sync.Mutex
		gotBody     string
		gotLength   int64
		gotEncoding []string
	)
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = string(b)
		gotLength = r.ContentLength
		gotEncoding = r.TransferEncoding
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"done":true}`)
	}))
	defer engine.Close()

	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "engine", engine.URL, "llama"))
	p := testProxy(disc, 11434)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"llama","stream":true,"messages":[]}`))
	// A chunked request: the client declared no length.
	req.ContentLength = -1
	req.TransferEncoding = []string{"chunked"}

	rec := httptest.NewRecorder()
	p.handleHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(gotBody, `"include_usage":true`) {
		t.Errorf("engine got body %q, want the usage request", gotBody)
	}
	if !strings.Contains(gotBody, `"messages":[]`) {
		t.Errorf("engine got body %q, want the whole request", gotBody)
	}
	// The framing the client used is kept: a request without a declared
	// length stays a request without a declared length, so the forwarded
	// body can never disagree with a length the upstream request claims.
	if gotLength != -1 {
		t.Errorf("engine saw Content-Length %d, want an unknown length", gotLength)
	}
	if len(gotEncoding) == 0 || gotEncoding[0] != "chunked" {
		t.Errorf("engine saw Transfer-Encoding %v, want chunked", gotEncoding)
	}
}

// TestHandleHTTP_FailoverForwardsUsageRequest: every failover attempt
// replays the same body, so the usage request survives the failover and
// both attempts carry the same request.
func TestHandleHTTP_FailoverForwardsUsageRequest(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []string
	)
	record := func(r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
	}
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":"loading model"}`)
	}))
	defer busy.Close()

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"done":true}`)
	}))
	defer good.Close()

	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "busy", busy.URL, "llama"))
	disc.AddManual(nodeForModel(t, "good", good.URL, "llama"))
	p := testProxy(disc, 11434)
	p.SetSelected("busy") // deterministic: busy is tried first

	rec := httptest.NewRecorder()
	p.handleHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"llama","stream":true,"messages":[]}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (should have failed over past the 503)", rec.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("the engine saw %d attempts, want 2", len(bodies))
	}
	for i, body := range bodies {
		if !strings.Contains(body, `"include_usage":true`) {
			t.Errorf("attempt %d got body %q, want the usage request", i+1, body)
		}
	}
	if bodies[0] != bodies[1] {
		t.Errorf("the two attempts got different bodies:\n%q\n%q", bodies[0], bodies[1])
	}
}
