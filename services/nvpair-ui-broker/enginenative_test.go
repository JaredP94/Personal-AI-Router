// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"nvpair-shared/enginenative"
	"nvpair-shared/noderec"
)

// TestBrokerCarriesTheEngineNativeView pins the broker's half of the
// engine-native path: the daemon's directory node carries the engine's own
// view, the broker copies it onto its own node record, and the client-facing
// snapshot carries it on the wire.
//
// The field is additive and optional. A node with no view omits it entirely,
// because an absent view means "not known" and an empty object would read as
// "the engine reported nothing".
func TestBrokerCarriesTheEngineNativeView(t *testing.T) {
	view := &enginenative.Stats{
		Engine:    enginenative.EngineOmlx,
		Supported: true,
		Totals: enginenative.Totals{
			Requests:         3270,
			PromptTokens:     212978711,
			CompletionTokens: 2147906,
			CachedTokens:     157045860,
			CacheEfficiency:  73.7,
		},
		Rates: enginenative.Rates{
			PrefillTokensPerSecond:    1553.2,
			GenerationTokensPerSecond: 32.5,
		},
		Models: []enginenative.Model{
			{Model: "mlx-community/Llama-3.2-3B-Instruct-4bit", Loaded: true},
		},
		ObservedAt: 1716998400000,
	}
	node := noderec.DirectoryNode{
		HostUUID:     "uuid-a",
		Name:         "host-a",
		IP:           "192.0.2.10",
		Models:       []string{"mlx-community/Llama-3.2-3B-Instruct-4bit"},
		EngineNative: view,
	}

	enriched := directoryToEnriched(node)
	if enriched.EngineNative == nil {
		t.Fatal("the broker's node record dropped the engine-native view")
	}
	if enriched.EngineNative.Totals.Requests != 3270 {
		t.Errorf("engine-native totals = %+v, want the engine's own counters", enriched.EngineNative.Totals)
	}

	store := newDiscoveryStore()
	store.Upsert(enriched, sourceScanner)
	snap := store.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("want one node in the snapshot, got %d", len(snap))
	}
	if snap[0].EngineNative == nil {
		t.Fatal("the client-facing node record carries no engine-native view")
	}
	if snap[0].EngineNative.Rates.GenerationTokensPerSecond != 32.5 {
		t.Errorf("engine-native rates = %+v, want the engine's own rates", snap[0].EngineNative.Rates)
	}

	raw, err := json.Marshal(snap[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"engineNative"`) {
		t.Errorf("the wire payload does not carry the engine-native view: %s", raw)
	}

	// A node with no view omits the field entirely.
	raw, err = json.Marshal(AvailableNode{ID: "host-b"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "engineNative") {
		t.Errorf("a node with no engine-native view still carried the field: %s", raw)
	}
}

// TestEngineNativeViewChangeIsAnnounced pins the change detection: a node
// whose engine's own counters moved is a change a consumer must hear about,
// and a re-read of the same numbers is not — the observation stamp moves on
// every read, but the numbers did not.
func TestEngineNativeViewChangeIsAnnounced(t *testing.T) {
	store := newDiscoveryStore()
	changes := 0
	store.SetOnChange(func() { changes++ })
	defer store.SetOnChange(nil)

	view := &enginenative.Stats{
		Engine:     enginenative.EngineOmlx,
		Supported:  true,
		Totals:     enginenative.Totals{Requests: 2},
		ObservedAt: 1000,
	}
	store.Upsert(EnrichedNode{ID: "host-a", HostUUID: "uuid-a", EngineNative: view}, sourceScanner)
	if changes != 1 {
		t.Fatalf("the first sighting reported %d changes, want 1", changes)
	}

	// The same numbers, read again: the stamp moved, the counters did not.
	same := *view
	same.ObservedAt = 2000
	store.Upsert(EnrichedNode{ID: "host-a", HostUUID: "uuid-a", EngineNative: &same}, sourceScanner)
	if changes != 1 {
		t.Fatalf("an unchanged engine-native view reported %d changes, want 1", changes)
	}

	// The engine's own counters moved: that is a change.
	moved := same
	moved.Totals.Requests = 3
	store.Upsert(EnrichedNode{ID: "host-a", HostUUID: "uuid-a", EngineNative: &moved}, sourceScanner)
	if changes != 2 {
		t.Fatalf("a changed engine-native view reported %d changes, want 2", changes)
	}
}
