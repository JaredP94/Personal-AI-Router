// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"net/http"
	"testing"
)

func TestRedactHeaders(t *testing.T) {
	headers := map[string][]string{
		"Authorization":   {"Bearer secret-token-12345"},
		"X-Pair-Pin":      {"123456"},
		"Content-Type":    {"application/json"},
		"X-Forwarded-For": {"192.168.1.10"},
	}
	clean := RedactHeaders(headers)
	if clean["Authorization"] != "[REDACTED]" {
		t.Errorf("expected Authorization to be redacted, got %s", clean["Authorization"])
	}
	if clean["X-Pair-Pin"] != "[REDACTED]" {
		t.Errorf("expected X-Pair-Pin to be redacted, got %s", clean["X-Pair-Pin"])
	}
	if clean["Content-Type"] != "application/json" {
		t.Errorf("expected Content-Type to be preserved, got %s", clean["Content-Type"])
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
	ctx := context.Background()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	InjectHTTPContext(ctx, req)
	extractedCtx := ExtractHTTPContext(req)
	if extractedCtx == nil {
		t.Fatal("expected non-nil context from ExtractHTTPContext")
	}
}
