// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"strings"
	"testing"

	"nvpair-tui/rpc"
)

func TestCacheAffinityDiagnostics(t *testing.T) {
	v := newProxiesView(nil)
	for _, params := range []string{
		`{"method":"POST","path":"/v1/chat/completions","cache_affinity":true}`,
		`{"method":"POST","path":"/v1/completions","cache_affinity":false}`,
		`{"method":"GET","path":"/v1/models","cache_affinity":true}`,
		`{"method":"POST","path":"/v1/chat/completions"}`,
	} {
		v.handleNotification(&rpc.Message{Method: "omlx-proxy:proxy/request", Params: json.RawMessage(params)})
	}
	output := v.View()
	if !strings.Contains(output, "oMLX") || !strings.Contains(output, "cache affinity=50.0% (1/2)") {
		t.Fatalf("missing oMLX inference-only cache affinity rate: %s", output)
	}
	if !strings.Contains(output, "cache affinity=n/a (0 requests)") {
		t.Fatalf("empty engines must not show a zero hit rate: %s", output)
	}
}
