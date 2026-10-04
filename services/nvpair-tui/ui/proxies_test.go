// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"strings"
	"testing"

	"nvpair-tui/rpc"
)

// The view no longer counts routing traffic. The proxy computes the totals
// (nvpair-shared/routingmetrics) and stamps them on every proxy/request event,
// so this test pins that the view replaces wholesale from the event rather than
// accumulating, and that unrouted traffic cannot inflate what is displayed.
//
// The previous version of this test fed four events with no totals and asserted
// 50.0% (1/2) — proving the view was re-implementing the routing filter. Two of
// those four events were a GET model list and a cache_affinity-less POST (the
// shape of a 502 rejection), and both were counted as routed requests.
func TestCacheAffinityDiagnostics(t *testing.T) {
	v := newProxiesView(nil)

	v.handleNotification(&rpc.Message{
		Method: "omlx-proxy:proxy/request",
		Params: json.RawMessage(`{"path":"/v1/chat/completions","cache_affinity":true,"requests":1,"cache_affinity_hits":1}`),
	})
	if out := v.View(); !strings.Contains(out, "cache affinity=100.0% (1/1)") {
		t.Fatalf("first event must set totals to 1/1: %s", out)
	}

	// Replacement semantics: a lower total must be adopted, not max()-ed or
	// incremented. This is what a proxy restart looks like to the view.
	v.handleNotification(&rpc.Message{
		Method: "omlx-proxy:proxy/request",
		Params: json.RawMessage(`{"path":"/api/generate","cache_affinity":false,"requests":3,"cache_affinity_hits":1}`),
	})
	out := v.View()
	if !strings.Contains(out, "oMLX") {
		t.Fatalf("missing oMLX engine row: %s", out)
	}
	if !strings.Contains(out, "cache affinity=33.3% (1/3)") {
		t.Fatalf("missing oMLX cache affinity rate: %s", out)
	}

	// An unrouted event (502 shape: totals absent) must preserve, not wipe.
	v.handleNotification(&rpc.Message{
		Method: "omlx-proxy:proxy/request",
		Params: json.RawMessage(`{"method":"POST","path":"/v1/chat/completions","status":502,"error":"no available node"}`),
	})
	if out := v.View(); !strings.Contains(out, "cache affinity=33.3% (1/3)") {
		t.Fatalf("unrouted event changed the totals: %s", out)
	}

	// A real proxy restart reports zeros explicitly, and that must reset.
	v.handleNotification(&rpc.Message{
		Method: "omlx-proxy:proxy/request",
		Params: json.RawMessage(`{"path":"/api/generate","cache_affinity":false,"requests":0,"cache_affinity_hits":0}`),
	})
	if out := v.View(); !strings.Contains(out, "cache affinity=n/a (0 requests)") {
		t.Fatalf("explicit zero totals did not reset the row: %s", out)
	}

	// Engines that have reported nothing show n/a, never a fake zero percent.
	if !strings.Contains(out, "cache affinity=n/a (0 requests)") {
		t.Fatalf("empty engines must not show a zero hit rate: %s", out)
	}
}

// Totals arriving on one engine must not leak onto another.
func TestCacheAffinityTotalsArePerEngine(t *testing.T) {
	v := newProxiesView(nil)
	v.handleNotification(&rpc.Message{
		Method: "lmstudio-proxy:proxy/request",
		Params: json.RawMessage(`{"path":"/v1/chat/completions","cache_affinity":true,"requests":7,"cache_affinity_hits":7}`),
	})
	out := v.View()
	if !strings.Contains(out, "cache affinity=100.0% (7/7)") {
		t.Fatalf("lm-studio totals not displayed: %s", out)
	}
	if v.engines[0].requests != 0 || v.engines[0].cacheAffinityHits != 0 {
		t.Fatalf("lm-studio totals leaked onto ollama: %+v", v.engines[0])
	}
	if v.engines[2].requests != 0 || v.engines[2].cacheAffinityHits != 0 {
		t.Fatalf("lm-studio totals leaked onto oMLX: %+v", v.engines[2])
	}
}
