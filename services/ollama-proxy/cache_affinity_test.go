// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nvpair-shared/prefixhash"
	"nvpair-shared/schedulerwire"
)

// A real conversation includes the assistant reply before the next user turn.
// Removing affinity promotion must make the second request follow scheduler B.
func TestCacheAffinityConversationAndFailover(t *testing.T) {
	failed := false
	engine := func(id string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if id == "a" && failed {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"%s\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", id)
		}))
	}
	a, b := engine("a"), engine("b")
	defer a.Close()
	defer b.Close()
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "a", a.URL, "model"))
	disc.AddManual(nodeForModel(t, "b", b.URL, "model"))
	rec := &recRW{}
	p := NewProxy(NewCodec(rec), disc, 1235)
	defer p.closeIdleTransports()
	messages := []map[string]string{{"role": "system", "content": "private-system-canary"}, {"role": "user", "content": "private-user-canary"}}
	for turn := 0; turn < 5; turn++ {
		if turn == 2 {
			failed = true
		}
		if turn == 0 {
			p.SetPriority([]string{"a", "b"})
		} else {
			p.SetPriority([]string{"b", "a"})
		}
		body, _ := json.Marshal(map[string]interface{}{"model": "model", "messages": messages, "stream": true})
		w := httptest.NewRecorder()
		p.handleHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body))))
		want := "a"
		if turn >= 2 {
			want = "b"
		}
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"content":"`+want+`"`) {
			t.Fatalf("turn %d routed incorrectly: status %d response %s", turn, w.Code, w.Body.String())
		}
		messages = append(messages, map[string]string{"role": "assistant", "content": want}, map[string]string{"role": "user", "content": "next-private-user-canary"})
	}
	if rec.has("private-system-canary") || rec.has("private-user-canary") {
		t.Fatal("prompt leaked into notifications")
	}
	if !rec.has(`"cache_affinity":true`) {
		t.Fatal("no affinity completion reported")
	}
}

func TestCacheAffinityRespectsRoutingGates(t *testing.T) {
	for _, tc := range []struct {
		name               string
		pending, pressure  int
		pin                string
		remove, otherModel bool
	}{
		{name: "pending", pending: 2}, {name: "pressure", pressure: 3}, {name: "pin", pin: "b"}, {name: "departed", remove: true}, {name: "inventory", otherModel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := func(id string) *httptest.Server {
				return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, id) }))
			}
			a, b := engine("a"), engine("b")
			defer a.Close()
			defer b.Close()
			disc := NewDiscovery()
			disc.AddManual(nodeForModel(t, "a", a.URL, "model"))
			disc.AddManual(nodeForModel(t, "b", b.URL, "model"))
			p := testProxy(disc, 1235)
			defer p.closeIdleTransports()
			p.SetPriority([]string{"a", "b"})
			request := func(body string) string {
				w := httptest.NewRecorder()
				p.handleHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
				return w.Body.String()
			}
			if got := request(`{"model":"model","messages":[{"role":"user","content":"first"}]}`); got != "a" {
				t.Fatal(got)
			}
			if tc.remove {
				disc.RemoveManual("a")
			}
			if tc.otherModel {
				disc.AddManual(nodeForModel(t, "a", a.URL, "different"))
			}
			p.SetSelected(tc.pin)
			p.SetPrioritySnapshot(schedulerwire.Priority{Nodes: []string{"b", "a"}, Ranks: []schedulerwire.NodeRank{{ID: "a", Pending: tc.pending, GPUPressure: tc.pressure}, {ID: "b"}}})
			if got := request(`{"model":"model","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"reply"},{"role":"user","content":"next"}]}`); got != "b" {
				t.Fatalf("gate routed to %q, want b", got)
			}
		})
	}
}

func TestCacheAffinityFailedResponseDoesNotRegister(t *testing.T) {
	for _, failure := range []string{"status", "client-write", "truncated"} {
		t.Run(failure, func(t *testing.T) {
			a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch failure {
				case "status":
					w.WriteHeader(400)
				case "truncated":
					w.Header().Set("Content-Length", "999")
				}
				io.WriteString(w, "partial")
			}))
			defer a.Close()
			b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "b") }))
			defer b.Close()
			disc := NewDiscovery()
			disc.AddManual(nodeForModel(t, "a", a.URL, "model"))
			disc.AddManual(nodeForModel(t, "b", b.URL, "model"))
			p := testProxy(disc, 1235)
			defer p.closeIdleTransports()
			p.SetPriority([]string{"a", "b"})
			first := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[{"role":"user","content":"first"}]}`))
			if failure == "client-write" {
				p.handleHTTP(&clientGoneWriter{err: io.ErrClosedPipe}, first)
			} else {
				p.handleHTTP(httptest.NewRecorder(), first)
			}
			p.SetPriority([]string{"b", "a"})
			w := httptest.NewRecorder()
			p.handleHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"reply"},{"role":"user","content":"next"}]}`)))
			if w.Body.String() != "b" {
				t.Fatalf("failed response established affinity: got %q", w.Body.String())
			}
		})
	}
}

func TestCacheAffinityCleanTruncatedSSEDoesNotRegister(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
	}))
	defer upstream.Close()

	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "a", upstream.URL, "model"))
	p := testProxy(disc, 1235)
	defer p.closeIdleTransports()
	body := []byte(`{"model":"model","messages":[{"role":"user","content":"first"}]}`)
	p.handleHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body))))
	context := prefixhash.Extract(http.MethodPost, "/v1/chat/completions", body)
	if _, ok := p.prt.Lookup("model", context.Full); ok {
		t.Fatal("cleanly truncated SSE stream registered a cache-affinity hint")
	}
}

func TestCacheAffinityOllamaStreamRequiresDone(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		registered bool
	}{
		{name: "truncated", body: `{"message":{"content":"partial"},"done":false}` + "\n"},
		{name: "complete", body: `{"message":{"content":"complete"},"done":true}` + "\n", registered: true},
		{name: "post-terminal", body: `{"message":{"content":"complete"},"done":true}` + "\n" + `{"message":{"content":"after-done"},"done":false}` + "\n"},
		{name: "oversized-post-terminal", body: `{"message":{"content":"complete"},"done":true}` + "\n" + strings.Repeat("x", maxCompletionLineBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer upstream.Close()

			disc := NewDiscovery()
			disc.AddManual(nodeForModel(t, "a", upstream.URL, "model"))
			p := testProxy(disc, 1235)
			defer p.closeIdleTransports()
			body := []byte(`{"model":"model","messages":[{"role":"user","content":"first"}],"stream":true}`)
			p.handleHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(string(body))))
			context := prefixhash.Extract(http.MethodPost, "/api/chat", body)
			_, registered := p.prt.Lookup("model", context.Full)
			if registered != tc.registered {
				t.Fatalf("registered %v, want %v", registered, tc.registered)
			}
		})
	}
}

func TestCacheAffinityReservationAndThreshold(t *testing.T) {
	p := prProxy(t)
	p.SetPriority([]string{"b", "a"})
	for i, want := range []bool{true, true, false} {
		candidates, affinity := p.reserveCandidateWithAffinity(reservationCandidates("b", "a"), "a")
		if affinity != want {
			t.Fatalf("reservation %d affinity %v want %v", i, affinity, want)
		}
		if want && candidates[0].id != "a" {
			t.Fatal("warm node was not promoted")
		}
	}
	p.SetPrioritySnapshot(schedulerwire.Priority{Nodes: []string{"b", "a"}, Ranks: []schedulerwire.NodeRank{{ID: "a", Pending: 1, GPUPressure: 2}}})
	candidates, affinity := p.reserveCandidateWithAffinity(reservationCandidates("b", "a"), "a")
	if !affinity || candidates[0].id != "a" {
		t.Fatal("pending 1 / pressure 2 should preserve affinity")
	}
}
