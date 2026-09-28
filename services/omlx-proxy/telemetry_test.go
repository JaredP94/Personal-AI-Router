// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	cfg := telemetry.DefaultConfig("nvpair-omlx-proxy-test")
	cfg.RecordPayloads = recordPayloads
	tel := telemetry.NewTestProvider(tp, cfg)

	var inBuf, outBuf bytes.Buffer
	rwc := dummyReadWriteCloser{Reader: &inBuf, Writer: &outBuf}
	codec := NewCodec(rwc)
	disc := NewDiscovery()
	p := NewProxy(codec, disc, 1234, tel)
	p.bufPool = bufpool.New()
	return p, exporter
}

func TestOMLXProxyTelemetrySpans(t *testing.T) {
	var capturedTraceparent string
	mockEngine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedTraceparent = r.Header.Get("traceparent")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// First streaming chunk (delta content)
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"mlx-community/Llama-3.2-3B-Instruct-4bit\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hi there\"},\"finish_reason\":null}]}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(10 * time.Millisecond)

		// Terminal chunk with token stats
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"mlx-community/Llama-3.2-3B-Instruct-4bit\",\"choices\":[],\"usage\":{\"prompt_tokens\":15,\"completion_tokens\":8}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer mockEngine.Close()

	p, exporter := setupTestProxyWithTelemetry(t, false)
	node := nodeForModel(t, "test-node-1", mockEngine.URL, "mlx-community/Llama-3.2-3B-Instruct-4bit")
	p.discovery.AddManual(node)
	p.SetSelected("test-node-1")

	reqBody := `{"model":"mlx-community/Llama-3.2-3B-Instruct-4bit","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqBody))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret-token-xyz")
	req.Header.Set("X-Custom-Client", "open-webui")
	rec := httptest.NewRecorder()

	p.handlePlain(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if capturedTraceparent == "" {
		t.Errorf("expected outgoing request to engine to have W3C traceparent injected")
	}

	spans := exporter.GetSpans()
	if len(spans) < 3 {
		t.Fatalf("expected at least 3 spans (schedule, dispatch, inference), got %d", len(spans))
	}

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
	if rootAttrs["gen_ai.request.model"] != "mlx-community/Llama-3.2-3B-Instruct-4bit" {
		t.Errorf("expected gen_ai.request.model = mlx-community/Llama-3.2-3B-Instruct-4bit, got %v", rootAttrs["gen_ai.request.model"])
	}
	if rootAttrs["pair.client.protocol"] != "openai" {
		t.Errorf("expected pair.client.protocol = openai, got %v", rootAttrs["pair.client.protocol"])
	}
	if rootAttrs["http.response.status_code"] != int64(200) {
		t.Errorf("expected http.response.status_code = 200, got %v", rootAttrs["http.response.status_code"])
	}
	if rootAttrs["http.request.header.authorization"] != "[REDACTED]" {
		t.Errorf("expected authorization header to be [REDACTED], got %v", rootAttrs["http.request.header.authorization"])
	}
	if rootAttrs["http.request.header.x-custom-client"] != "open-webui" {
		t.Errorf("expected custom header preserved, got %v", rootAttrs["http.request.header.x-custom-client"])
	}
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

func TestOMLXProxyPayloadCaptureEnabled(t *testing.T) {
	mockEngine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"mlx-community/Llama-3.2-3B-Instruct-4bit","choices":[{"index":0,"message":{"role":"assistant","content":"Hello world response"},"finish_reason":"stop"}]}` + "\n"))
	}))
	defer mockEngine.Close()

	p, exporter := setupTestProxyWithTelemetry(t, true) // RecordPayloads: true
	node := nodeForModel(t, "test-node-1", mockEngine.URL, "mlx-community/Llama-3.2-3B-Instruct-4bit")
	p.discovery.AddManual(node)
	p.SetSelected("test-node-1")

	reqBody := `{"model":"mlx-community/Llama-3.2-3B-Instruct-4bit","prompt":"Tell me a story"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/completions", bytes.NewBufferString(reqBody))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.handlePlain(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

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

func TestOMLXProxyCandidateFailureTelemetry(t *testing.T) {
	// Candidate 0: closes immediately so dial fails
	badServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	badServer.Close()

	// Candidate 1: succeeds
	goodServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"mlx-community/Llama-3.2-3B-Instruct-4bit","choices":[{"index":0,"message":{"role":"assistant","content":"success from candidate 1"},"finish_reason":"stop"}]}` + "\n"))
	}))
	defer goodServer.Close()

	p, exporter := setupTestProxyWithTelemetry(t, false)
	badNode := nodeForModel(t, "bad-node", badServer.URL, "mlx-community/Llama-3.2-3B-Instruct-4bit")
	goodNode := nodeForModel(t, "good-node", goodServer.URL, "mlx-community/Llama-3.2-3B-Instruct-4bit")
	p.discovery.AddManual(badNode)
	p.discovery.AddManual(goodNode)
	p.SetSelected("bad-node") // bad-node will be tried first

	reqBody := `{"model":"mlx-community/Llama-3.2-3B-Instruct-4bit","messages":[{"role":"user","content":"test failover"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqBody))
	req.RemoteAddr = "127.0.0.1:54321"
	rec := httptest.NewRecorder()

	p.handlePlain(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected failover to succeed with 200, got %d", rec.Code)
	}

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

func attrMap(kvs []attribute.KeyValue) map[string]any {
	out := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		out[string(kv.Key)] = kv.Value.AsInterface()
	}
	return out
}
