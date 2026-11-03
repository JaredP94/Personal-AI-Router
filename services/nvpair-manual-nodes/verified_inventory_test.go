// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"testing"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
)

func TestPublicManualInventoryCannotClaimPinnedPrincipal(t *testing.T) {
	dir := t.TempDir()
	clustertrusttest.Join(t, dir, "test-cluster", "self", "peer")
	m, _, rt := newTestManager(t)
	m.mesh = clustertrust.Open(dir)
	entry := ManualEntry{Address: "forged-peer"}
	id := nodeID(entry)
	m.nodes[id] = &trackedNode{entry: entry}
	rt.set("GET", "forged-peer:14318", "/v1/node-info", func(*http.Request) (*http.Response, error) {
		return httpJSON(200, `{"hostUuid":"peer","clusterUuid":"peer","telemetryValid":true,"GPUs":[{"name":"forged","utilization_percent":99}]}`)
	})
	rt.set("GET", "forged-peer:14322", "/v1/models", func(*http.Request) (*http.Response, error) {
		return httpJSON(200, `{"models":["forged-model"],"modelsByEngine":{"omlx":["forged-model"]}}`)
	})
	m.probeNode(entry)
	got := m.nodes[id].status
	if got.IdentityVerification != "unverified" || got.HostUUID != "" || got.ClusterUUID != "" || got.TelemetryValid || len(got.GPUs) > 0 || len(got.Models) > 0 {
		t.Fatalf("public inventory accepted for service decisions: %+v", got)
	}
}

// A deliberately configured bare engine is manual inventory, not a PAIR peer.
// It may advertise models without acquiring a host identity or GPU telemetry.
func TestBareEnginesUseExplicitManualInventory(t *testing.T) {
	m, rw, rt := newTestManager(t)
	entry := ManualEntry{Kind: "engine", Name: "bare-engines", Address: "bare.local"}
	rt.set(http.MethodGet, "bare.local:14318", "/v1/node-info", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusNotFound, `{}`)
	})
	rt.set(http.MethodGet, "bare.local:11434", "/", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{}`)
	})
	rt.set(http.MethodGet, "bare.local:11434", "/api/tags", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{"models":[{"name":"llama3"}]}`)
	})
	configureHealthyLMStudio(rt, entry.Address, []string{"qwen"})
	configureHealthyOMLX(rt, entry.Address, []string{"mlx"})
	initial := m.addNode(entry)
	if initial.OllamaPort != 11434 || initial.NodeInfoPort != 14318 {
		t.Fatalf("default ports not set: %+v", initial)
	}
	got := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, rw, methodIs("node/discovered")))
	if got.IdentityVerification != "manual" || got.HostUUID != "" || got.ClusterUUID != "" || got.PeerRecordID != "" || got.NodeInfoUp || got.TelemetryValid || len(got.GPUs) != 0 {
		t.Fatalf("bare engine acquired PAIR identity or telemetry: %+v", got)
	}
	if !got.OllamaUp || !got.LMStudioUp || !got.OMLXUp || len(got.Models) != 3 {
		t.Fatalf("bare engine availability or models lost: %+v", got)
	}
	for engine, want := range map[string]string{"ollama": "llama3", "lmstudio": "qwen", "omlx": "mlx"} {
		if models := got.ModelsByEngine[engine]; len(models) != 1 || models[0] != want {
			t.Errorf("manual %s models = %v, want [%s]", engine, models, want)
		}
	}
}
