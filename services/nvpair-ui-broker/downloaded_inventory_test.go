// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"nvpair-shared/clustertrusttest"
	"nvpair-shared/noderec"
)

func TestManualCacheErrorSurvivesDiscoveryAndRelayUntilRecovery(t *testing.T) {
	b := inventoryTestBroker(t)
	status := manualNodeStatus{
		ID: "manual-peer", Address: "192.0.2.2", HostUUID: "peer", ClusterUUID: "peer",
		PeerRecordID: b.mesh.PeerRecordID("peer"), IdentityVerification: "verified",
		PeerMetadata:           &noderec.PeerMetadataParams{HostUUID: "peer", Name: "peer", Services: map[noderec.ServiceKey]int{noderec.ServiceOllama: 11434}},
		Models:                 []string{"ollama-model", "lmstudio-model"},
		ModelsByEngine:         map[string][]string{"ollama": {"ollama-model"}, "lmstudio": {"lmstudio-model"}},
		DownloadErrorsByEngine: map[string]string{"llamacpp": "cache_unavailable"},
	}
	publications := 0
	b.store.onChange = func() { publications++ }
	b.upsertManualNode(status)
	nodes := b.store.Snapshot()
	if len(nodes) != 1 || !nodes[0].Trusted || len(nodes[0].Models) != 2 || nodes[0].DownloadErrorsByEngine["llamacpp"] != "cache_unavailable" {
		t.Fatalf("manual cache error damaged discovery: %+v", nodes)
	}
	relayed := b.relayDir.Snapshot(noderec.ServiceOllama)
	if len(relayed) != 1 || len(relayed[0].Models) != 2 || relayed[0].DownloadErrorsByEngine["llamacpp"] != "cache_unavailable" {
		t.Fatalf("manual cache error damaged relay: %+v", relayed)
	}
	before := publications
	status.DownloadErrorsByEngine = nil
	b.upsertManualNode(status)
	nodes = b.store.Snapshot()
	relayed = b.relayDir.Snapshot(noderec.ServiceOllama)
	if publications != before+1 || len(nodes) != 1 || len(relayed) != 1 || len(nodes[0].Models) != 2 || len(nodes[0].DownloadErrorsByEngine) != 0 || len(relayed[0].DownloadErrorsByEngine) != 0 {
		t.Fatalf("cache recovery was not published: nodes=%+v relay=%+v", nodes, relayed)
	}
	status.DownloadErrorsByEngine = map[string]string{"llamacpp": "cache_unavailable"}
	clustertrusttest.RemovePeerPin(t, b.clusterDir, "peer")
	if retired := b.normalizeManualStatus(status); len(retired.DownloadErrorsByEngine) != 0 || len(retired.Models) != 0 {
		t.Fatalf("retired proof retained inventory diagnostics: %+v", retired)
	}
}

func TestDownloadedInventoryDiscoveryProjection(t *testing.T) {
	node := noderec.DirectoryNode{HostUUID: "peer", Name: "peer", DownloadedByEngine: map[string][]noderec.DownloadedModel{"llamacpp": {{ID: "catalog-id", File: "cache.gguf", Size: 24, Owned: true}}}}
	enriched := directoryToEnriched(node)
	available := (storedNode{scanner: &enriched}).toAvailable()
	if rows := available.DownloadedByEngine["llamacpp"]; len(rows) != 1 || rows[0].File != "cache.gguf" || !rows[0].Owned {
		t.Fatalf("lost cache metadata: %+v", available)
	}
	withError := node
	withError.DownloadErrorsByEngine = map[string]string{"llamacpp": "cache_unavailable"}
	projectedError := directoryToEnriched(withError)
	if (storedNode{scanner: &projectedError}).toAvailable().DownloadErrorsByEngine["llamacpp"] != "cache_unavailable" || enrichedNodeEqual(enriched, projectedError) {
		t.Fatal("cache error was lost or did not change discovery snapshot")
	}
	changed := enriched
	changed.DownloadedByEngine = map[string][]noderec.DownloadedModel{"llamacpp": {}}
	if enrichedNodeEqual(enriched, changed) {
		t.Fatal("cache deletion did not change discovery snapshot")
	}
}
