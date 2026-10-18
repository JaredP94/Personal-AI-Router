// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/telemetry"
)

// Flush the real manager's batch processor without adding a production test API.
func flushTelemetry(t *testing.T, p *Proxy) {
	t.Helper()
	_, span := p.telemetry.Session().Tracer().Start(context.Background(), "test-flush")
	defer span.End()
	if tp, ok := span.TracerProvider().(*sdktrace.TracerProvider); ok {
		if err := tp.ForceFlush(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func unconfiguredTelemetryProxy(t *testing.T) (*Proxy, *tracetest.InMemoryExporter, *prRec) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OMLX_API_KEY", "")
	exporter := tracetest.NewInMemoryExporter()
	nextExporter := exporter
	m := telemetry.NewManager("test-omlx-proxy", func(context.Context, string) (sdktrace.SpanExporter, error) {
		current := nextExporter
		nextExporter = tracetest.NewInMemoryExporter()
		return current, nil
	})
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	rec := &prRec{}
	return NewProxy(NewCodec(rec), NewDiscovery(), 1235, m), exporter, rec
}

func telemetryRPC(t *testing.T, p *Proxy, rec *prRec, method, params string) Message {
	t.Helper()
	id := json.RawMessage(`123`)
	rec.mu.Lock()
	start := len(rec.b)
	rec.mu.Unlock()
	p.handleMessage(&Message{JSONRPC: "2.0", ID: &id, Method: method, Params: json.RawMessage(params)})
	rec.mu.Lock()
	data := bytes.Clone(rec.b[start:])
	rec.mu.Unlock()
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		var msg Message
		if json.Unmarshal(line, &msg) == nil && msg.ID != nil && string(*msg.ID) == "123" {
			return msg
		}
	}
	t.Fatalf("no RPC response for %s", method)
	return Message{}
}

func configureTelemetry(t *testing.T, p *Proxy, rec *prRec, params string, want uint64) {
	t.Helper()
	msg := telemetryRPC(t, p, rec, "telemetry/configure", params)
	var result struct {
		Revision uint64 `json:"revision"`
	}
	if msg.Error != nil || json.Unmarshal(msg.Result, &result) != nil || result.Revision != want {
		t.Fatalf("configure = %s, error %v; want revision %d", msg.Result, msg.Error, want)
	}
}

func telemetryRequest(p *Proxy, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/completions", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	p.handlePlain(rec, req)
	return rec
}

func telemetryEngine(t *testing.T, p *Proxy, response string) {
	t.Helper()
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, response+"\n")
	}))
	t.Cleanup(engine.Close)
	p.discovery.AddManual(nodeForModel(t, "engine", engine.URL, "llama"))
}

func TestUnconfiguredProxyExportsNothing(t *testing.T) {
	p, exporter, _ := unconfiguredTelemetryProxy(t)
	telemetryEngine(t, p, `{"choices":[{"text":"hello","finish_reason":"stop"}]}`)
	if got := telemetryRequest(p, `{"model":"llama","prompt":"hello"}`); got.Code != 200 {
		t.Fatalf("status = %d", got.Code)
	}
	flushTelemetry(t, p)
	if got := len(exporter.GetSpans()); got != 0 {
		t.Fatalf("unconfigured proxy exported %d spans", got)
	}
}

func TestConfigureEnablesAndStaleRevisionIgnored(t *testing.T) {
	p, exporter, rec := unconfiguredTelemetryProxy(t)
	telemetryEngine(t, p, `{"choices":[{"text":"hello","finish_reason":"stop"}]}`)
	configureTelemetry(t, p, rec, `{"revision":2,"enabled":true,"collectorPort":4317,"recordPayloads":false}`, 2)
	for i := 0; i < 2; i++ {
		if i == 1 {
			configureTelemetry(t, p, rec, `{"revision":1,"enabled":false,"collectorPort":4317,"recordPayloads":false}`, 2)
		}
		exporter.Reset()
		if got := telemetryRequest(p, `{"model":"llama","prompt":"hello"}`); got.Code != 200 {
			t.Fatal(got.Code)
		}
		flushTelemetry(t, p)
		found := false
		for _, span := range exporter.GetSpans() {
			found = found || span.Name == "pair.proxy.inference"
		}
		if !found {
			t.Fatal("configured proxy did not export inference")
		}
	}
	msg := telemetryRPC(t, p, rec, "telemetry/status", `{}`)
	var status map[string]json.RawMessage
	if msg.Error != nil || json.Unmarshal(msg.Result, &status) != nil {
		t.Fatalf("status: %s %v", msg.Result, msg.Error)
	}
	if len(status) != 5 || string(status["revision"]) != "2" || string(status["enabled"]) != "true" || string(status["lastExportError"]) != "null" || string(status["lastExportErrorAt"]) != "null" {
		t.Fatalf("status = %s", msg.Result)
	}
	var lastOK string
	if json.Unmarshal(status["lastExportOk"], &lastOK) != nil {
		t.Fatal("missing export success time")
	}
	if stamp, err := time.Parse(time.RFC3339, lastOK); err != nil || stamp.Location() != time.UTC {
		t.Fatalf("invalid UTC timestamp %q", lastOK)
	}
}

func TestTelemetryConfigureRejectsInvalidParams(t *testing.T) {
	p, _, rec := unconfiguredTelemetryProxy(t)
	for _, params := range []string{
		`null`, `{}`, `[]`, `{"revision":-1,"enabled":true,"collectorPort":4317,"recordPayloads":false}`,
		`{"revision":1,"enabled":true,"collectorPort":0,"recordPayloads":false}`,
		`{"revision":1,"enabled":true,"collectorPort":65536,"recordPayloads":false}`,
		`{"revision":1,"enabled":false,"collectorPort":4317,"recordPayloads":true}`,
		`{"revision":1,"enabled":"true","collectorPort":4317,"recordPayloads":false}`,
		`{"revision":1,"enabled":null,"collectorPort":4317,"recordPayloads":false}`,
	} {
		msg := telemetryRPC(t, p, rec, "telemetry/configure", params)
		if msg.Error == nil || msg.Error.Code != -32602 {
			t.Fatalf("params %s: error = %v", params, msg.Error)
		}
	}
	msg := telemetryRPC(t, p, rec, "telemetry/status", `{}`)
	var status map[string]json.RawMessage
	if msg.Error != nil || json.Unmarshal(msg.Result, &status) != nil || string(status["revision"]) != "0" || string(status["enabled"]) != "false" || string(status["lastExportOk"]) != "null" {
		t.Fatalf("invalid requests changed status: %s", msg.Result)
	}
}

func TestDisableMidStreamDropsCompletion(t *testing.T) {
	p, exporter, rec := unconfiguredTelemetryProxy(t)
	configureTelemetry(t, p, rec, `{"revision":1,"enabled":true,"collectorPort":4317,"recordPayloads":true}`, 1)
	first := make(chan struct{})
	finish := make(chan struct{})
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"text":"before-revoke","finish_reason":null}]}`+"\n\n")
		w.(http.Flusher).Flush()
		close(first)
		<-finish
		_, _ = io.WriteString(w, `data: {"choices":[{"text":"after-revoke","finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	defer engine.Close()
	defer func() {
		select {
		case <-finish:
		default:
			close(finish)
		}
	}()
	p.discovery.AddManual(nodeForModel(t, "engine", engine.URL, "llama"))
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- telemetryRequest(p, `{"model":"llama","prompt":"hello","stream":true}`) }()
	<-first
	configureTelemetry(t, p, rec, `{"revision":2,"enabled":true,"collectorPort":4317,"recordPayloads":false}`, 2)
	close(finish)
	if got := <-done; got.Code != 200 || !strings.Contains(got.Body.String(), "after-revoke") {
		t.Fatal("stream interrupted by capture revocation")
	}
	flushTelemetry(t, p)
	for _, span := range exporter.GetSpans() {
		for _, a := range span.Attributes {
			if a.Key == "gen_ai.completion" || a.Key == "output.value" {
				t.Fatal("completion survived revocation")
			}
		}
	}
}

// The manager also gates revoked exports. Inspect the reader's span through an
// independent processor so that gate cannot mask an erroneous finalize capture.
func TestTelemetryBodyReaderRevokesBufferedCompletion(t *testing.T) {
	p, _, rec := unconfiguredTelemetryProxy(t)
	configureTelemetry(t, p, rec, `{"revision":1,"enabled":true,"collectorPort":4317,"recordPayloads":true}`, 1)
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer tp.Shutdown(context.Background())
	_, span := tp.Tracer("reader").Start(context.Background(), "reader")
	r := newTelemetryBodyReader(io.NopCloser(strings.NewReader("")), span, time.Now(), p.telemetry.Session())
	r.observe([]byte(`data: {"choices":[{"delta":{"content":"before"}}]}` + "\n"))
	if r.completionBuf.Len() == 0 {
		t.Fatal("fixture did not buffer completion")
	}
	configureTelemetry(t, p, rec, `{"revision":2,"enabled":true,"collectorPort":4317,"recordPayloads":false}`, 2)
	r.observe([]byte(`data: {"choices":[{"delta":{"content":"after"}}]}` + "\n"))
	if r.completionBuf.Len() != 0 {
		t.Fatal("revoked reader retained completion")
	}
	r.Close()
	span.End()
	for _, a := range exporter.GetSpans()[0].Attributes {
		if a.Key == "gen_ai.completion" || a.Key == "output.value" {
			t.Fatal("revoked reader finalized payload")
		}
	}
}

func TestSensitiveHeadersNeverExported(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(map[bool]string{false: "metadata", true: "payloads"}[capture], func(t *testing.T) {
			p, exporter := setupTestProxyWithTelemetry(t, capture)
			telemetryEngine(t, p, `{"choices":[{"text":"hello","finish_reason":"stop"}]}`)
			req := httptest.NewRequest(http.MethodPost, "/v1/completions?api_key=url-secret", strings.NewReader(`{"model":"llama","prompt":"hello"}`))
			req.RemoteAddr = "127.0.0.1:1"
			for k, v := range map[string]string{"Authorization": "Bearer auth-secret", "Cookie": "cookie-secret", "X-Secret": "custom-secret"} {
				req.Header.Set(k, v)
			}
			p.handlePlain(httptest.NewRecorder(), req)
			flushTelemetry(t, p)
			if len(exporter.GetSpans()) < 3 {
				t.Fatal("no inference spans")
			}
			for _, span := range exporter.GetSpans() {
				for _, a := range span.Attributes {
					text := strings.ToLower(string(a.Key) + a.Value.Emit())
					for _, secret := range []string{"authorization", "cookie", "x-secret", "api_key", "auth-secret", "cookie-secret", "custom-secret", "url-secret"} {
						if strings.Contains(text, secret) {
							t.Errorf("exported sensitive header or URL query: %s", secret)
						}
					}
				}
			}
		})
	}
}

func TestPayloadTruncation(t *testing.T) {
	p, exporter := setupTestProxyWithTelemetry(t, true)
	// Exactly 20 KiB, with a two-byte rune crossing the 16 KiB boundary.
	long := strings.Repeat("a", (16<<10)-1) + "é" + strings.Repeat("b", (4<<10)-1)
	responseModel := strings.Repeat("a", 255) + "é" + strings.Repeat("b", 143)
	response, _ := json.Marshal(map[string]interface{}{"choices": []map[string]string{{"text": long, "finish_reason": "stop"}}, "model": responseModel})
	telemetryEngine(t, p, string(response))
	body, _ := json.Marshal(map[string]string{"model": "llama", "prompt": long})
	if got := telemetryRequest(p, string(body)); got.Code != 200 {
		t.Fatal(got.Code)
	}
	flushTelemetry(t, p)
	seen := map[string]bool{}
	for _, span := range exporter.GetSpans() {
		attrs := attrMap(span.Attributes)
		for _, key := range []string{"gen_ai.prompt", "input.value", "gen_ai.completion", "output.value"} {
			if s, ok := attrs[key].(string); ok {
				seen[key] = true
				if len(s) > 16<<10 || !utf8.ValidString(s) {
					t.Errorf("%s is not bounded UTF-8", key)
				}
			}
		}
		if span.Name == "pair.proxy.inference" && attrs["gen_ai.prompt.truncated"] != true {
			t.Error("missing prompt truncated flag")
		}
		if span.Name == "pair.engine.dispatch" {
			if attrs["gen_ai.completion.truncated"] != true {
				t.Error("missing completion truncated flag")
			}
			if s, ok := attrs["gen_ai.response.model"].(string); !ok || len(s) > 256 || !utf8.ValidString(s) {
				t.Error("response model not bounded")
			}
		}
	}
	if len(seen) != 4 {
		t.Fatalf("missing captured attributes: %v", seen)
	}
}

func TestNoPeerTraceContext(t *testing.T) {
	p, exporter := setupTestProxyWithTelemetry(t, true)
	dir := t.TempDir()
	clustertrusttest.Join(t, dir, "test-cluster", "self", "peer")
	p.mesh = clustertrust.Open(dir)
	peer, peerExporter := setupTestProxyWithTelemetry(t, true)
	peer.mesh = p.mesh
	pinData, err := os.ReadFile(filepath.Join(dir, "trusted", "peer.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pin struct {
		CertPEM string `json:"certPem"`
	}
	if err := json.Unmarshal(pinData, &pin); err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(pin.CertPEM))
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("traceparent") != "" || r.Header.Get("tracestate") != "" || r.Header.Get("baggage") != "" {
			t.Error("trace context reached ingress engine")
		}
		_, _ = io.WriteString(w, `{"choices":[{"text":"hi","finish_reason":"stop"}]}`+"\n")
	}))
	defer backend.Close()
	local := nodeForModel(t, "local", backend.URL, "llama")
	peer.setLocalBackend(localBackend{Host: "127.0.0.1", Port: local.Port, Healthy: true})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("traceparent") != "" || r.Header.Get("tracestate") != "" || r.Header.Get("baggage") != "" {
			t.Error("trace context sent to peer")
		}
		// Authenticate through the real pin verifier without building a second
		// key infrastructure; this certificate is the temporary trusted leaf.
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
		peer.handleClusterIngress(w, r)
	}))
	defer server.Close()
	node := nodeForModel(t, "peer-node", server.URL, "llama")
	node.ClusterUUID = "peer"
	p.discovery.SetSubscribed([]Node{node})
	p.peerTransports = map[string]*http.Transport{"peer": server.Client().Transport.(*http.Transport)}
	req := httptest.NewRequest(http.MethodPost, "/v1/completions", strings.NewReader(`{"model":"llama","prompt":"hello"}`))
	req.RemoteAddr = "127.0.0.1:1"
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	req.Header.Set("tracestate", "vendor=state")
	req.Header.Set("baggage", "secret=state")
	rec := httptest.NewRecorder()
	p.handlePlain(rec, req)
	if rec.Code != 200 {
		t.Fatalf("peer dispatch failed: %d %s", rec.Code, rec.Body.String())
	}
	// A peer can also supply its own tracing headers. The terminal ingress
	// must neither extract them nor pass them on to the loopback engine.
	ingressReq := httptest.NewRequest(http.MethodPost, "/v1/completions", strings.NewReader(`{"model":"llama","prompt":"peer request"}`))
	ingressReq.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	ingressReq.Header = req.Header.Clone()
	ingressRec := httptest.NewRecorder()
	peer.handleClusterIngress(ingressRec, ingressReq)
	if ingressRec.Code != 200 {
		t.Fatalf("direct ingress failed: %d", ingressRec.Code)
	}
	flushTelemetry(t, p)
	flushTelemetry(t, peer)
	for _, span := range peerExporter.GetSpans() {
		if span.Name != "test-flush" {
			t.Errorf("ingress created %s", span.Name)
		}
	}
	found := false
	for _, span := range exporter.GetSpans() {
		if span.Name == "pair.proxy.inference" {
			found = true
			if span.SpanContext.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" || span.Parent.SpanID().String() != "00f067aa0ba902b7" {
				t.Error("local client trace context not extracted")
			}
		}
	}
	if !found {
		t.Fatal("missing local inference span")
	}
}

func TestTelemetryStatusExportFailure(t *testing.T) {
	p, _, rec := unconfiguredTelemetryProxy(t)
	p.telemetry = telemetry.NewManager("failure", func(context.Context, string) (sdktrace.SpanExporter, error) { return failingTelemetryExporter{}, nil })
	defer p.telemetry.Shutdown(context.Background())
	configureTelemetry(t, p, rec, `{"revision":1,"enabled":true,"collectorPort":4317,"recordPayloads":false}`, 1)
	_, span := p.telemetry.Session().Tracer().Start(context.Background(), "export-failure")
	span.End()
	tp := span.TracerProvider().(*sdktrace.TracerProvider)
	if err := tp.ForceFlush(context.Background()); err == nil {
		t.Fatal("expected failed export")
	}
	msg := telemetryRPC(t, p, rec, "telemetry/status", `{}`)
	var status struct {
		LastOK *string `json:"lastExportOk"`
		Error  *string `json:"lastExportError"`
		At     *string `json:"lastExportErrorAt"`
	}
	if msg.Error != nil || json.Unmarshal(msg.Result, &status) != nil || status.LastOK != nil || status.Error == nil || *status.Error != "collector unavailable" || status.At == nil {
		t.Fatalf("failure status = %s", msg.Result)
	}
	if stamp, err := time.Parse(time.RFC3339, *status.At); err != nil || stamp.Location() != time.UTC {
		t.Error("failure timestamp not RFC3339 UTC")
	}
}

type failingTelemetryExporter struct{}

func (failingTelemetryExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return errors.New("collector unavailable")
}
func (failingTelemetryExporter) Shutdown(context.Context) error { return nil }
