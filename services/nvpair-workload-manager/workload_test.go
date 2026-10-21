// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseLifecycleValid(t *testing.T) {
	params := json.RawMessage(`{"workloadInfo":{"id":"wl-1","model":"llama-3","engine":"trt-llm","state":"queued","originatedFrom":"node-A","createdAt":1,"startedAt":null,"completedAt":null,"error":null,"requesterId":null}}`)
	w, err := parseLifecycle(params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w.ID != "wl-1" || w.Model != "llama-3" || w.Engine != "trt-llm" {
		t.Fatalf("unexpected workload: %+v", w)
	}
}

func TestParseLifecycleRejectsMissingFields(t *testing.T) {
	cases := map[string]string{
		"no workloadInfo":      `{}`,
		"empty id":             `{"workloadInfo":{"id":"","model":"m","engine":"e","state":"queued","originatedFrom":"n"}}`,
		"empty engine":         `{"workloadInfo":{"id":"x","model":"m","engine":"","state":"queued","originatedFrom":"n"}}`,
		"empty state":          `{"workloadInfo":{"id":"x","model":"m","engine":"e","state":"","originatedFrom":"n"}}`,
		"empty originatedFrom": `{"workloadInfo":{"id":"x","model":"m","engine":"e","state":"queued","originatedFrom":""}}`,
	}
	for name, body := range cases {
		if _, err := parseLifecycle(json.RawMessage(body)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

// TestPeerLifecycleKeepsMeasurementOnUpsert covers the peer -> broker path: a
// lifecycle frame that arrives from another node carries the engine's
// measurement, and re-emitting it as workloads:upsert must not drop it.
func TestPeerLifecycleKeepsMeasurementOnUpsert(t *testing.T) {
	params := json.RawMessage(`{"workloadInfo":{"id":"7","model":"m","engine":"omlx","runId":"r",` +
		`"state":"completed","originatedFrom":"node-b","scheduledOn":"node-b",` +
		`"createdAt":1,"prefillMs":1250,"decodeMs":4500,"prefillTokens":120,"decodeTokens":34}}`)
	wl, err := parseLifecycle(params)
	if err != nil {
		t.Fatal(err)
	}
	if wl.PrefillMs == nil || *wl.PrefillMs != 1250 {
		t.Fatalf("prefill ms: %v", wl.PrefillMs)
	}
	if wl.DecodeMs == nil || *wl.DecodeMs != 4500 {
		t.Fatalf("decode ms: %v", wl.DecodeMs)
	}
	out, err := json.Marshal(lifecycleParams{WorkloadInfo: wl})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"prefillMs":1250`) {
		t.Fatalf("measurement must survive the upsert re-marshal: %s", out)
	}
}

// TestParseLifecycleWithoutMeasurement checks that the measurement stays
// optional: a frame without one is still accepted and re-marshals without
// inventing measurement fields.
func TestParseLifecycleWithoutMeasurement(t *testing.T) {
	params := json.RawMessage(`{"workloadInfo":{"id":"8","model":"m","engine":"omlx",` +
		`"state":"completed","originatedFrom":"node-b","createdAt":1}}`)
	wl, err := parseLifecycle(params)
	if err != nil {
		t.Fatalf("a lifecycle frame without a measurement must still be valid: %v", err)
	}
	if wl.PrefillMs != nil || wl.DecodeMs != nil || wl.PrefillTokens != nil || wl.DecodeTokens != nil {
		t.Fatalf("expected no measurement, got %+v", wl.Measurement)
	}
	out, err := json.Marshal(lifecycleParams{WorkloadInfo: wl})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"prefillMs", "decodeMs", "prefillTokens", "decodeTokens"} {
		if strings.Contains(string(out), field) {
			t.Fatalf("absent measurement must not be emitted: %s", out)
		}
	}
}

func TestParseRemove(t *testing.T) {
	id, node, err := parseRemove(json.RawMessage(`{"workloadId":"wl-9","originatedFrom":"node-C"}`))
	if err != nil || id != "wl-9" || node != "node-C" {
		t.Fatalf("expected wl-9/node-C, got %q/%q err=%v", id, node, err)
	}
	// originatedFrom is optional (backward compatible): a legacy payload
	// without it still parses, with an empty originatedFrom.
	id, node, err = parseRemove(json.RawMessage(`{"workloadId":"wl-9"}`))
	if err != nil || id != "wl-9" || node != "" {
		t.Fatalf("expected wl-9/empty, got %q/%q err=%v", id, node, err)
	}
	if _, _, err := parseRemove(json.RawMessage(`{"workloadId":""}`)); err == nil {
		t.Fatal("expected error for empty workloadId")
	}
}

func TestLifecycleMethodMapping(t *testing.T) {
	for method, want := range map[string]WorkloadState{
		MethodSubmitted: StateQueued,
		MethodStarted:   StateRunning,
		MethodCompleted: StateCompleted,
		MethodErrored:   StateFailed,
	} {
		if !isLifecycleMethod(method) {
			t.Errorf("%s should be a lifecycle method", method)
		}
		if lifecycleMethods[method] != want {
			t.Errorf("%s mapped to %q, want %q", method, lifecycleMethods[method], want)
		}
	}
	if isLifecycleMethod(MethodRemove) {
		t.Error("workloads:remove is not a lifecycle method")
	}
	if isLifecycleMethod("bogus:method") {
		t.Error("unknown method should not be a lifecycle method")
	}
}
