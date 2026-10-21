// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"nvpair-shared/bufpool"
	"nvpair-shared/telemetry"
)

type dummyReadWriteCloser struct {
	io.Reader
	io.Writer
}

func (d dummyReadWriteCloser) Close() error { return nil }

func setupTestProxyWithTelemetry(t *testing.T, recordPayloads bool) (*Proxy, *tracetest.InMemoryExporter) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	tel := telemetry.NewManager("nvpair-ollama-proxy-test", func(context.Context, string) (sdktrace.SpanExporter, error) {
		return exporter, nil
	})
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	if _, err := tel.Apply(context.Background(), 1, telemetry.Settings{
		Enabled: true, CollectorPort: 4317, RecordPayloads: recordPayloads,
	}); err != nil {
		t.Fatal(err)
	}

	var inBuf, outBuf bytes.Buffer
	rwc := dummyReadWriteCloser{Reader: &inBuf, Writer: &outBuf}
	codec := NewCodec(rwc)
	disc := NewDiscovery()
	p := NewProxy(codec, disc, 11435)
	p.telemetry = tel
	p.bufPool = bufpool.New()
	return p, exporter
}

func TestOllamaProxyTelemetrySpans(t *testing.T) {
	mockEngine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)

		// First streaming chunk
		_, _ = w.Write([]byte(`{"model":"llama3.2:3b","message":{"role":"assistant","content":"Hi there"},"done":false}` + "\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(10 * time.Millisecond)

		// Terminal chunk with token stats
		_, _ = w.Write([]byte(`{"model":"llama3.2:3b","done":true,"prompt_eval_count":15,"eval_count":8}` + "\n"))
	}))
	defer mockEngine.Close()

	p, exporter := setupTestProxyWithTelemetry(t, false)
	node := nodeForModel(t, "test-node-1", mockEngine.URL, "llama3.2:3b")
	p.discovery.AddManual(node)
	p.SetSelected("test-node-1")

	reqBody := `{"model":"llama3.2:3b","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewBufferString(reqBody))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret-token-xyz")
	req.Header.Set("X-Custom-Client", "open-webui")
	rec := httptest.NewRecorder()

	p.handlePlain(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	flushTelemetry(t, p)
	spans := exporter.GetSpans()
	if len(spans) < 3 {
		t.Fatalf("expected at least 3 spans (schedule, dispatch, inference), got %d", len(spans))
	}

	// Verify spans by name
	spanMap := make(map[string]tracetest.SpanStub)
	for _, s := range spans {
		spanMap[s.Name] = s
	}

	// 1. Root inference span
	rootSpan, ok := spanMap["pair.proxy.inference"]
	if !ok {
		t.Fatalf("missing span 'pair.proxy.inference'")
	}
	rootAttrs := attrMap(rootSpan.Attributes)
	if rootAttrs["gen_ai.system"] != "pair" {
		t.Errorf("expected gen_ai.system = pair, got %v", rootAttrs["gen_ai.system"])
	}
	if rootAttrs["gen_ai.request.model"] != "llama3.2:3b" {
		t.Errorf("expected gen_ai.request.model = llama3.2:3b, got %v", rootAttrs["gen_ai.request.model"])
	}
	if rootAttrs["pair.client.protocol"] != "ollama" {
		t.Errorf("expected pair.client.protocol = ollama, got %v", rootAttrs["pair.client.protocol"])
	}
	if rootAttrs["http.response.status_code"] != int64(200) {
		t.Errorf("expected http.response.status_code = 200, got %v", rootAttrs["http.response.status_code"])
	}
	if rootAttrs["http.request.header.content-type"] != "application/json" {
		t.Errorf("expected allowlisted content-type, got %v", rootAttrs["http.request.header.content-type"])
	}
	// Ensure prompt is NOT recorded when RecordPayloads=false
	if _, hasPrompt := rootAttrs["gen_ai.prompt"]; hasPrompt {
		t.Errorf("expected gen_ai.prompt to be omitted when RecordPayloads=false")
	}

	// 2. Scheduler span
	schedSpan, ok := spanMap["pair.router.schedule"]
	if !ok {
		t.Fatalf("missing span 'pair.router.schedule'")
	}
	schedAttrs := attrMap(schedSpan.Attributes)
	if schedAttrs["pair.route.selected_node_id"] != "test-node-1" {
		t.Errorf("expected selected node test-node-1, got %v", schedAttrs["pair.route.selected_node_id"])
	}

	// 3. Dispatch span
	dispatchSpan, ok := spanMap["pair.engine.dispatch"]
	if !ok {
		t.Fatalf("missing span 'pair.engine.dispatch'")
	}
	dispatchAttrs := attrMap(dispatchSpan.Attributes)
	if dispatchAttrs["gen_ai.usage.input_tokens"] != int64(15) {
		t.Errorf("expected input_tokens = 15, got %v", dispatchAttrs["gen_ai.usage.input_tokens"])
	}
	if dispatchAttrs["gen_ai.usage.output_tokens"] != int64(8) {
		t.Errorf("expected output_tokens = 8, got %v", dispatchAttrs["gen_ai.usage.output_tokens"])
	}
	if ttft, hasTTFT := dispatchAttrs["pair.inference.ttft_ms"]; !hasTTFT || ttft.(int64) < 0 {
		t.Errorf("expected valid pair.inference.ttft_ms, got %v", ttft)
	}
}

func TestOllamaProxyPayloadCaptureEnabled(t *testing.T) {
	mockEngine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"model":"llama3.2:3b","response":"Hello world response","done":true}` + "\n"))
	}))
	defer mockEngine.Close()

	p, exporter := setupTestProxyWithTelemetry(t, true) // RecordPayloads: true
	node := nodeForModel(t, "test-node-1", mockEngine.URL, "llama3.2:3b")
	p.discovery.AddManual(node)
	p.SetSelected("test-node-1")

	reqBody := `{"model":"llama3.2:3b","prompt":"Tell me a story"}`
	req := httptest.NewRequest(http.MethodPost, "/api/generate", bytes.NewBufferString(reqBody))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.handlePlain(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	flushTelemetry(t, p)
	spans := exporter.GetSpans()
	spanMap := make(map[string]tracetest.SpanStub)
	for _, s := range spans {
		spanMap[s.Name] = s
	}

	rootSpan := spanMap["pair.proxy.inference"]
	rootAttrs := attrMap(rootSpan.Attributes)
	if rootAttrs["input.value"] != "Tell me a story" {
		t.Errorf("expected input.value captured, got %v", rootAttrs["input.value"])
	}

	dispatchSpan := spanMap["pair.engine.dispatch"]
	dispatchAttrs := attrMap(dispatchSpan.Attributes)
	if dispatchAttrs["output.value"] != "Hello world response" {
		t.Errorf("expected output.value captured, got %v", dispatchAttrs["output.value"])
	}
}

func TestOllamaProxyCandidateFailureTelemetry(t *testing.T) {
	// Candidate 0: closes immediately so dial fails
	badServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	badServer.Close()

	// Candidate 1: succeeds
	goodServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"model":"llama3.2:3b","response":"success from candidate 1","done":true}` + "\n"))
	}))
	defer goodServer.Close()

	p, exporter := setupTestProxyWithTelemetry(t, false)
	badNode := nodeForModel(t, "bad-node", badServer.URL, "llama3.2:3b")
	goodNode := nodeForModel(t, "good-node", goodServer.URL, "llama3.2:3b")
	p.discovery.AddManual(badNode)
	p.discovery.AddManual(goodNode)
	p.SetSelected("bad-node") // bad-node will be tried first

	reqBody := `{"model":"llama3.2:3b","messages":[{"role":"user","content":"test failover"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewBufferString(reqBody))
	req.RemoteAddr = "127.0.0.1:54321"
	rec := httptest.NewRecorder()

	p.handlePlain(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected failover to succeed with 200, got %d", rec.Code)
	}

	flushTelemetry(t, p)
	spans := exporter.GetSpans()
	var dispatchSpans []tracetest.SpanStub
	for _, s := range spans {
		if s.Name == "pair.engine.dispatch" {
			dispatchSpans = append(dispatchSpans, s)
		}
	}

	if len(dispatchSpans) < 2 {
		t.Fatalf("expected at least 2 dispatch spans due to failover, got %d", len(dispatchSpans))
	}

	// First dispatch span should be the failed candidate with error status
	failedSpan := dispatchSpans[0]
	failedAttrs := attrMap(failedSpan.Attributes)
	if failedAttrs["http.response.status_code"] == int64(200) {
		t.Errorf("failed candidate dispatch span should NOT have http.response.status_code = 200")
	}
	if failedSpan.Status.Code.String() != "Error" {
		t.Errorf("failed candidate dispatch span status = %v, want Error", failedSpan.Status.Code)
	}

	// Second dispatch span should be the successful candidate
	successSpan := dispatchSpans[1]
	successAttrs := attrMap(successSpan.Attributes)
	if successAttrs["http.response.status_code"] != int64(200) {
		t.Errorf("successful candidate dispatch span status_code = %v, want 200", successAttrs["http.response.status_code"])
	}
	if successAttrs["pair.route.failover"] != true {
		t.Errorf("expected pair.route.failover = true on second attempt")
	}
}

// ollamaEngineWith serves body as an Ollama NDJSON response.
func ollamaEngineWith(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWorkloadCarriesOllamaMeasurement(t *testing.T) {
	srv := ollamaEngineWith(t, `{"model":"llama3.2:3b","done":true,"prompt_eval_count":15,`+
		`"eval_count":8,"prompt_eval_duration":1200000000,"eval_duration":4000000000}`+"\n")
	rec := &recRW{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "serving-node", srv.URL, "llama3.2:3b"))
	p := NewProxy(NewCodec(rec), disc, 11434)

	p.handleHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"model":"llama3.2:3b"}`)))

	for _, want := range []string{
		`"method":"workload:completed"`,
		`"prefillTokens":15`, `"decodeTokens":8`,
		`"prefillMs":1200`, `"decodeMs":4000`,
	} {
		if !rec.has(want) {
			t.Fatalf("workload event missing %s", want)
		}
	}
	// Ollama reports no time to first token and no cached-token count, so
	// those two stay absent rather than becoming zeros. It reports no total
	// time either: the measurement carries engine-reported values only, so
	// the proxy's own wall-clock duration must not stand in for it.
	for _, absent := range []string{`"ttftMs"`, `"cachedTokens"`, `"totalMs"`} {
		if rec.has(absent) {
			t.Fatalf("ollama reports no %s; it must stay absent", absent)
		}
	}
}

func TestOllamaMeasurementPresentWithoutTelemetry(t *testing.T) {
	// unconfiguredTelemetryProxy's manager is never enabled, so the session's
	// tracer is a noop and no span is recording. The measurement must still
	// reach the workload event.
	p, _, rec := unconfiguredTelemetryProxy(t)
	srv := ollamaEngineWith(t, `{"model":"llama3.2:3b","done":true,"prompt_eval_count":15,`+
		`"eval_count":8,"prompt_eval_duration":1200000000,"eval_duration":4000000000}`+"\n")
	p.discovery.AddManual(nodeForModel(t, "serving-node", srv.URL, "llama3.2:3b"))

	p.handleHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"model":"llama3.2:3b"}`)))

	if !rec.has(`"prefillMs":1200`) {
		t.Fatal("measurement missing with telemetry disabled")
	}
}

func TestOllamaUnreportedPhasesStayAbsent(t *testing.T) {
	// Ollama's counts but no durations: the phase fields stay absent.
	srv := ollamaEngineWith(t, `{"model":"llama3.2:3b","done":true,"prompt_eval_count":15,"eval_count":8}`+"\n")
	rec := &recRW{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "serving-node", srv.URL, "llama3.2:3b"))
	p := NewProxy(NewCodec(rec), disc, 11434)

	p.handleHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"model":"llama3.2:3b"}`)))

	if !rec.has(`"prefillTokens":15`) || !rec.has(`"decodeTokens":8`) {
		t.Fatal("token counts missing")
	}
	for _, absent := range []string{`"prefillMs"`, `"decodeMs"`, `"ttftMs"`, `"cachedTokens"`} {
		if rec.has(absent) {
			t.Fatalf("an unreported phase must stay absent: %s", absent)
		}
	}
}

func TestOllamaMeasuredZeroTokenCountIsKept(t *testing.T) {
	// A count Ollama reported as zero is a measured zero, not an absent value.
	srv := ollamaEngineWith(t, `{"model":"llama3.2:3b","done":true,"prompt_eval_count":0,"eval_count":0}`+"\n")
	rec := &recRW{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "serving-node", srv.URL, "llama3.2:3b"))
	p := NewProxy(NewCodec(rec), disc, 11434)

	p.handleHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"model":"llama3.2:3b"}`)))

	if !rec.has(`"prefillTokens":0`) {
		t.Fatal("a reported zero prefill count must be preserved")
	}
	if !rec.has(`"decodeTokens":0`) {
		t.Fatal("a reported zero decode count must be preserved")
	}
}

func attrMap(kvs []attribute.KeyValue) map[string]any {
	out := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		out[string(kv.Key)] = kv.Value.AsInterface()
	}
	return out
}
