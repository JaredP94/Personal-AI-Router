// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package noderec

import (
	"encoding/json"
	"strings"
	"testing"

	"nvpair-shared/enginenative"
)

// TestDirectoryNodeCarriesTheEngineNativeView pins the wire contract of the
// engine-native field on the node record: a node that has a view carries it
// under "engineNative", and a node that has none omits the key entirely.
//
// The omission is the load-bearing half. An absent view means "not known" —
// the engine publishes no admin API, the fetch failed, or the peer is not
// pinned — and an empty object in its place would read as "the engine
// reported nothing", which is a different claim.
func TestDirectoryNodeCarriesTheEngineNativeView(t *testing.T) {
	view := &enginenative.Stats{
		Engine:    enginenative.EngineOmlx,
		Supported: true,
		Totals: enginenative.Totals{
			Requests:         2,
			PromptTokens:     120,
			CompletionTokens: 34,
			CachedTokens:     90,
			CacheEfficiency:  75,
		},
		Rates: enginenative.Rates{
			PrefillTokensPerSecond:    100,
			GenerationTokensPerSecond: 50,
		},
		Models:     []enginenative.Model{{Model: "mlx-community/Llama-3.2-3B-Instruct-4bit", Loaded: true}},
		ObservedAt: 1716998400000,
	}
	raw, err := json.Marshal(DirectoryNode{HostUUID: "host-a", EngineNative: view})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	carried, ok := fields["engineNative"]
	if !ok {
		t.Fatalf("a node with an engine-native view did not carry it: %s", raw)
	}
	var got enginenative.Stats
	if err := json.Unmarshal(carried, &got); err != nil {
		t.Fatal(err)
	}
	if got.Engine != enginenative.EngineOmlx || !got.Supported {
		t.Errorf("engine-native view = %+v, want the omlx view", got)
	}
	if got.Totals.Requests != 2 || got.Totals.PromptTokens != 120 || got.Totals.CacheEfficiency != 75 {
		t.Errorf("engine-native totals = %+v, want the fetched counters", got.Totals)
	}
	if got.Rates.GenerationTokensPerSecond != 50 {
		t.Errorf("engine-native rates = %+v, want the fetched rates", got.Rates)
	}
	if len(got.Models) != 1 || got.Models[0].Model != "mlx-community/Llama-3.2-3B-Instruct-4bit" {
		t.Errorf("engine-native models = %+v, want the engine's own list", got.Models)
	}

	// A node with no view omits the key: absence is "not known", never an
	// empty object that would read as "reported nothing".
	raw, err = json.Marshal(DirectoryNode{HostUUID: "host-b"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "engineNative") {
		t.Errorf("a node with no engine-native view still carried the field: %s", raw)
	}
}
