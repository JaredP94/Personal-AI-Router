// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"net/http"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestRedactHeaders(t *testing.T) {
	tests := []struct {
		name     string
		headers  map[string][]string
		expected map[string]string
	}{
		{
			name: "standard sensitive headers",
			headers: map[string][]string{
				"Authorization":       {"Bearer secret-token-12345"},
				"Proxy-Authorization": {"Basic dXNlcjpwYXNz"},
				"Cookie":              {"session=abc"},
				"Set-Cookie":          {"session=xyz; Secure"},
				"X-Pair-Pin":          {"123456"},
				"X-Api-Key":           {"key-789"},
				"Proxy-Auth":          {"secret"},
			},
			expected: map[string]string{
				"Authorization":       "[REDACTED]",
				"Proxy-Authorization": "[REDACTED]",
				"Cookie":              "[REDACTED]",
				"Set-Cookie":          "[REDACTED]",
				"X-Pair-Pin":          "[REDACTED]",
				"X-Api-Key":           "[REDACTED]",
				"Proxy-Auth":          "[REDACTED]",
			},
		},
		{
			name: "case-insensitivity check",
			headers: map[string][]string{
				"AUTHORIZATION":       {"Bearer upper"},
				"proxy-authorization": {"Basic lower"},
				"sEt-cOoKiE":          {"val"},
				"COOKIE":              {"val"},
				"x-PAIR-pin":          {"999"},
				"X-API-KEY":           {"key-upper"},
				"PROXY-AUTH":          {"secret-upper"},
			},
			expected: map[string]string{
				"AUTHORIZATION":       "[REDACTED]",
				"proxy-authorization": "[REDACTED]",
				"sEt-cOoKiE":          "[REDACTED]",
				"COOKIE":              "[REDACTED]",
				"x-PAIR-pin":          "[REDACTED]",
				"X-API-KEY":           "[REDACTED]",
				"PROXY-AUTH":          "[REDACTED]",
			},
		},
		{
			name: "non-sensitive headers preserved",
			headers: map[string][]string{
				"Content-Type":    {"application/json"},
				"X-Forwarded-For": {"192.168.1.10"},
				"User-Agent":      {"curl/7.68.0"},
			},
			expected: map[string]string{
				"Content-Type":    "application/json",
				"X-Forwarded-For": "192.168.1.10",
				"User-Agent":      "curl/7.68.0",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clean := RedactHeaders(tt.headers)
			for k, expectedVal := range tt.expected {
				if got, ok := clean[k]; !ok || got != expectedVal {
					t.Errorf("header %q: expected %q, got %q", k, expectedVal, got)
				}
			}
		})
	}
}

func TestDualModePayloadControl(t *testing.T) {
	prompt := "Hello, how are you?"
	completion := "I am an AI model."

	// Mode 1: RecordPayloads = false
	attrsFalse := FormatPayloadAttributes(false, prompt, completion)
	if _, ok := attrsFalse["gen_ai.prompt"]; ok {
		t.Errorf("expected prompt to be absent when RecordPayloads=false")
	}
	if _, ok := attrsFalse["gen_ai.completion"]; ok {
		t.Errorf("expected completion to be absent when RecordPayloads=false")
	}

	// Mode 2: RecordPayloads = true
	attrsTrue := FormatPayloadAttributes(true, prompt, completion)
	if attrsTrue["input.value"] != prompt || attrsTrue["output.value"] != completion {
		t.Errorf("expected prompt and completion to be present when RecordPayloads=true")
	}
}

func TestProviderLifecycle(t *testing.T) {
	ctx := context.Background()
	cfg := DefaultConfig("test-service")
	cfg.Enabled = false // disabled mode test

	p, err := Init(ctx, cfg)
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	if p.Tracer() == nil {
		t.Error("expected non-nil tracer")
	}
	if p.RecordPayloads() {
		t.Error("expected RecordPayloads to be false")
	}
	if err := p.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown failed: %v", err)
	}
}

func TestHTTPContextPropagation(t *testing.T) {
	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	if err != nil {
		t.Fatalf("invalid trace ID: %v", err)
	}
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	if err != nil {
		t.Fatalf("invalid span ID: %v", err)
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	if !sc.IsValid() {
		t.Fatal("expected constructed SpanContext to be valid")
	}

	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	InjectHTTPContext(ctx, req)
	traceparent := req.Header.Get("traceparent")
	if traceparent == "" {
		t.Fatal("expected traceparent header to be injected, got empty")
	}

	extractedCtx := ExtractHTTPContext(req)
	if extractedCtx == nil {
		t.Fatal("expected non-nil context from ExtractHTTPContext")
	}

	extractedSpanCtx := trace.SpanContextFromContext(extractedCtx)
	if !extractedSpanCtx.IsValid() {
		t.Errorf("expected extracted SpanContext to be valid, got %+v", extractedSpanCtx)
	}
	if extractedSpanCtx.TraceID() != traceID {
		t.Errorf("expected trace ID %s, got %s", traceID, extractedSpanCtx.TraceID())
	}
	if extractedSpanCtx.SpanID() != spanID {
		t.Errorf("expected span ID %s, got %s", spanID, extractedSpanCtx.SpanID())
	}
}

func TestHTTPContextPropagationDefensive(t *testing.T) {
	ctx := context.Background()

	// Test InjectHTTPContext with nil req (no panic)
	InjectHTTPContext(ctx, nil)

	// Test InjectHTTPContext with nil req.Header (no panic, creates header)
	reqNilHeader := &http.Request{Method: http.MethodGet}
	InjectHTTPContext(ctx, reqNilHeader)
	if reqNilHeader.Header == nil {
		t.Error("expected InjectHTTPContext to allocate req.Header when nil")
	}

	// Test ExtractHTTPContext with nil req (no panic, returns non-nil background context)
	ctxFromNilReq := ExtractHTTPContext(nil)
	if ctxFromNilReq == nil {
		t.Error("expected non-nil context from ExtractHTTPContext(nil)")
	}

	// Test ExtractHTTPContext with nil req.Header (no panic, returns req.Context())
	type contextKey string
	const key contextKey = "testKey"
	baseCtx := context.WithValue(context.Background(), key, "testVal")
	reqNilHeaderExtract, err := http.NewRequestWithContext(baseCtx, http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	reqNilHeaderExtract.Header = nil
	extractedFromNilHeader := ExtractHTTPContext(reqNilHeaderExtract)
	if extractedFromNilHeader == nil {
		t.Fatal("expected non-nil context from ExtractHTTPContext(reqNilHeaderExtract)")
	}
	if val := extractedFromNilHeader.Value(key); val != "testVal" {
		t.Errorf("expected context value %q, got %v", "testVal", val)
	}
}
