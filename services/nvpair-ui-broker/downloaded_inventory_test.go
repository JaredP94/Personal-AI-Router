// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"nvpair-shared/noderec"
	"testing"
)

func TestDownloadedInventoryDiscoveryProjection(t *testing.T) {
	node := noderec.DirectoryNode{HostUUID: "peer", Name: "peer", DownloadedByEngine: map[string][]noderec.DownloadedModel{"llamacpp": {{ID: "catalog-id", File: "cache.gguf", Size: 24, Owned: true}}}}
	enriched := directoryToEnriched(node)
	available := (storedNode{scanner: &enriched}).toAvailable()
	if rows := available.DownloadedByEngine["llamacpp"]; len(rows) != 1 || rows[0].File != "cache.gguf" || !rows[0].Owned {
		t.Fatalf("lost cache metadata: %+v", available)
	}
	changed := enriched
	changed.DownloadedByEngine = map[string][]noderec.DownloadedModel{"llamacpp": {}}
	if enrichedNodeEqual(enriched, changed) {
		t.Fatal("cache deletion did not change discovery snapshot")
	}
}
