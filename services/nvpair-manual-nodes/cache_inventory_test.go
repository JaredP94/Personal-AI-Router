// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"testing"

	"nvpair-shared/engineinventorytest"
)

func TestPinnedManualPeerCacheErrorPreservesServedInventory(t *testing.T) {
	m, _, rt := newTestManager(t)
	peer := newInventoryPeer(t, m, rt, "cache.local", "cache-peer")
	port, manifest, stop := engineinventorytest.Start(t, peer.clusterDir)
	peer.enginePort = port
	configureHealthyNode(rt, "cache.local", []string{"ollama-model", "lmstudio-model"}, sampleInfo())
	entry := peer.track(m, "cache-peer")
	m.probeNode(entry)
	before := m.nodes[nodeID(entry)].status
	if len(before.Models) != 2 || !before.OllamaUp || !before.LMStudioUp {
		t.Fatalf("fixture: %+v", before)
	}
	if err := os.WriteFile(manifest, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	m.probeNode(entry)
	got := m.nodes[nodeID(entry)].status
	if got.IdentityVerification != "verified" || got.PeerRecordID != before.PeerRecordID || len(got.Models) != 2 || !got.OllamaUp || !got.LMStudioUp || got.LlamaCPPUp {
		t.Fatalf("cache error damaged peer: %+v", got)
	}
	raw, _ := json.Marshal(got)
	var wire struct {
		Errors map[string]string `json:"downloadErrorsByEngine"`
	}
	_ = json.Unmarshal(raw, &wire)
	if wire.Errors["llamacpp"] != "cache_unavailable" || len(wire.Errors) != 1 {
		t.Fatalf("cache error lost: %s", raw)
	}
	if err := os.Remove(manifest); err != nil {
		t.Fatal(err)
	}
	m.probeNode(entry)
	raw, _ = json.Marshal(m.nodes[nodeID(entry)].status)
	wire.Errors = nil
	_ = json.Unmarshal(raw, &wire)
	if len(wire.Errors) != 0 {
		t.Fatalf("recovered cache retained error: %s", raw)
	}
	stop()
	m.probeNode(entry)
	got = m.nodes[nodeID(entry)].status
	if got.IdentityVerification != "verified" || got.OllamaUp || got.LMStudioUp || len(got.Models) != 0 {
		t.Fatalf("whole-inventory failure changed semantics: %+v", got)
	}
}
