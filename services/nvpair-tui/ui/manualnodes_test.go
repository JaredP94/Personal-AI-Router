// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"strings"
	"testing"
)

func TestManualPairPersistsBeforeInviting(t *testing.T) {
	transport, c := engineWire(t)
	v := newManualView(transport.client)
	v.SetSize(100, 24)
	v.Update(engineKey("a"))
	v.input.SetValue("  Workstation.tail.example  ")
	saved := engineRequest(t, v.submitAdd(), c, "node/add", `{"address":"Workstation.tail.example","kind":"pair"}`, `{"id":"manual:Workstation.tail.example","kind":"pair"}`)
	invite := v.Update(saved)
	result := engineRequest(t, invite, c, "cluster:invite-node", `{"address":"Workstation.tail.example"}`, `{"state":"pending","pin":"123456"}`)
	v.Update(result)
	if !strings.Contains(v.View(), "123456") {
		t.Fatalf("PIN not shown: %s", v.View())
	}
}

func TestManualEngineChoiceIsExplicitAndDoesNotPair(t *testing.T) {
	transport, c := engineWire(t)
	v := newManualView(transport.client)
	v.SetSize(100, 24)
	v.Update(engineKey("e"))
	if !strings.Contains(v.View(), "trusts no PAIR claims") {
		t.Fatal("engine bypass not explained")
	}
	v.input.SetValue("engine.example")
	saved := engineRequest(t, v.submitAdd(), c, "node/add", `{"address":"engine.example","kind":"engine"}`, `{"id":"manual:engine.example","kind":"engine"}`)
	if _, ok := saved.(manualActionMsg); !ok {
		t.Fatalf("engine triggered pairing: %T", saved)
	}
}

func TestManualViewListsOnlyConfiguredEntriesWithIntent(t *testing.T) {
	v := newManualView(nil)
	v.SetSize(100, 24)
	v.setNodes([]manualNode{{ID: "auto", FromCluster: true}, {ID: "overlay", Kind: "pair", Address: "overlay.example", IdentityVerification: "unverified"}, {ID: "engine", Kind: "engine", IdentityVerification: "manual"}})
	if len(v.nodes) != 2 || !strings.Contains(v.View(), "unverified") || !strings.Contains(v.View(), "engine / manual") {
		t.Fatalf("manual listing = %s", v.View())
	}
}
