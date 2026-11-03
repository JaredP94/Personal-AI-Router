// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Replaying the durable configuration into a fresh worker must not infer intent
// from whichever service happens to answer at that address after a restart.
func TestPairKindSurvivesWorkerRestartWithoutPairEndpoint(t *testing.T) {
	configuration := []byte(`{"address":"overlay.example","kind":"pair"}`)
	for restart := 0; restart < 2; restart++ {
		m, rw, rt := newTestManager(t)
		var entry ManualEntry
		if err := json.Unmarshal(configuration, &entry); err != nil {
			t.Fatal(err)
		}
		rt.set(http.MethodGet, "overlay.example:11434", "/", func(*http.Request) (*http.Response, error) { return httpJSON(200, `{}`) })
		rt.set(http.MethodGet, "overlay.example:11434", "/api/tags", func(*http.Request) (*http.Response, error) {
			return httpJSON(200, `{"models":[{"name":"wrong-machine"}]}`)
		})
		m.addNode(entry)
		got := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, rw, methodIs("node/discovered")))
		if got.IdentityVerification != "unverified" || got.OllamaUp || got.LMStudioUp || got.OMLXUp || len(got.Models) != 0 || got.PeerRecordID != "" {
			t.Fatalf("restart %d downgraded PAIR configuration to bare engine: %+v", restart, got)
		}
	}
}

func TestPairHostnameResolvesWithoutChangingConfiguration(t *testing.T) {
	m, rw, rt := newTestManager(t)
	peer := newInventoryPeer(t, m, rt, "overlay", "peer")
	configureHealthyNode(rt, "overlay", []string{"model"}, sampleInfo())
	peer.entry.Address = "localhost"
	peer.entry.Kind = "pair"
	m.addNode(peer.entry)
	got := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, rw, methodIs("node/discovered")))
	if got.Address != "localhost" || got.IdentityVerification != "verified" || got.HostUUID != "peer" || len(got.Models) == 0 {
		t.Fatalf("hostname did not resolve over pinned mTLS: %+v", got)
	}
}

func TestManualAddressValidationPreservesHostnames(t *testing.T) {
	for _, address := range []string{"Workstation.tail.example", "100.64.1.2", "2001:db8::1", "node.local."} {
		e := ManualEntry{Address: address, Kind: "pair"}
		if err := validateManualEntry(&e); err != nil || e.Address != address {
			t.Errorf("address %q: %+v %v", address, e, err)
		}
	}
	for _, address := range []string{"host:14318", "https://host", "host/path", "bad host", "-bad.example", ""} {
		e := ManualEntry{Address: address, Kind: "pair"}
		if err := validateManualEntry(&e); err == nil {
			t.Errorf("accepted malformed address %q", address)
		}
	}
	e := ManualEntry{Address: "overlay.example"}
	if err := validateManualEntry(&e); err != nil || e.Kind != "pair" {
		t.Fatalf("missing intent must be pair: %+v %v", e, err)
	}
}

func TestEngineKindIgnoresPairClaims(t *testing.T) {
	m, rw, rt := newTestManager(t)
	var entry ManualEntry
	if err := json.Unmarshal([]byte(`{"address":"engine.example","kind":"engine"}`), &entry); err != nil {
		t.Fatal(err)
	}
	rt.set(http.MethodGet, "engine.example:14318", "/v1/node-info", func(*http.Request) (*http.Response, error) {
		t.Error("explicit engine must not read PAIR claims")
		return httpJSON(200, `{"hostUuid":"claim"}`)
	})
	rt.set(http.MethodGet, "engine.example:11434", "/", func(*http.Request) (*http.Response, error) { return httpJSON(200, `{}`) })
	rt.set(http.MethodGet, "engine.example:11434", "/api/tags", func(*http.Request) (*http.Response, error) { return httpJSON(200, `{"models":[{"name":"model"}]}`) })
	m.addNode(entry)
	got := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, rw, methodIs("node/discovered")))
	if got.IdentityVerification != "manual" || !got.OllamaUp || got.HostUUID != "" || got.ClusterUUID != "" || got.PeerRecordID != "" || got.NodeInfoUp || got.TelemetryValid {
		t.Fatalf("engine intent not respected: %+v", got)
	}
}
