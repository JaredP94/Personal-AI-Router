// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"strings"
	"testing"
)

func TestLlamaPendingModelsDoNotLeakAcrossTargets(t *testing.T) {
	for _, key := range []string{"m", "b"} {
		t.Run(key, func(t *testing.T) {
			v, c := engineWire(t)
			modelEngine(v, "llamacpp")
			pending := v.Update(engineKey(key))
			if !strings.Contains(v.View(), "Loading models…") {
				t.Fatal("request did not enter loading state")
			}
			next := v.Update(engineKey("n"))
			v.Update(engineRequest(t, next, c, "engine:remote-get-installed", `{"node":"peer-uuid"}`, `{"engines":[{"engine":"llamacpp","capabilities":{"list_models":true,"pull_model":true}}]}`))
			v.Update(discoverySnapshotMsg{nodes: []availableNode{{HostUUID: "peer-uuid", Trusted: true, IdentityVerification: "verified", PeerRecordID: "proof", DownloadedByEngine: map[string][]llamaModel{"llamacpp": {{ID: "peer-model", File: "peer.gguf", Size: 24, Owned: true, SHA256: "verified"}}}}}})
			v.Update(engineKey("m"))
			method, params := "engine:action", `{"engine":"llamacpp","action":"list_downloaded"}`
			if key == "b" {
				method, params = "engine:model-catalog", `{"query":""}`
			}
			v.Update(engineRequest(t, pending, c, method, params, `{"models":[{"id":"stale-local-model","file":"stale.gguf"}]}`))
			if strings.Contains(v.View(), "Loading models…") || !strings.Contains(v.View(), "peer-model") || strings.Contains(v.View(), "stale-local-model") {
				t.Fatalf("pending local request contaminated peer pane: %s", v.View())
			}
		})
	}
}

func TestLlamaPendingCatalogDoesNotLeakIntoReopenedPeerInventory(t *testing.T) {
	v, c := engineWire(t)
	next := v.Update(engineKey("n"))
	v.Update(engineRequest(t, next, c, "engine:remote-get-installed", `{"node":"peer-uuid"}`, `{"engines":[{"engine":"llamacpp","capabilities":{"list_models":true,"pull_model":true}}]}`))
	v.Update(discoverySnapshotMsg{nodes: []availableNode{{HostUUID: "peer-uuid", Trusted: true, IdentityVerification: "verified", PeerRecordID: "proof", DownloadedByEngine: map[string][]llamaModel{"llamacpp": {{ID: "peer-model", File: "peer.gguf", Size: 24, Owned: true, SHA256: "verified"}}}}}})
	pending := v.Update(engineKey("b"))
	v.Update(engineKey("m")) // close the pending catalog
	v.Update(engineKey("m")) // reopen cached peer inventory
	v.Update(engineRequest(t, pending, c, "engine:model-catalog", `{"query":""}`, `{"models":[{"id":"stale-catalog-model","file":"stale.gguf"}]}`))
	if strings.Contains(v.View(), "Loading models…") || !strings.Contains(v.View(), "peer-model") || strings.Contains(v.View(), "stale-catalog-model") || strings.Contains(v.View(), "Catalog —") {
		t.Fatalf("pending catalog contaminated reopened peer pane: %s", v.View())
	}
}
