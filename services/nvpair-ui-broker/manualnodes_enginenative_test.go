// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"nvpair-shared/enginenative"
)

// The engine's own view of a manual node: the broker projects the view the
// manual-node prober read, so a manual node and a discovered node present the
// engine's counters identically.
//
// The node-scanner fetches the engine's view for the nodes it discovers over
// mDNS. A manual node is by definition a node this host reaches by address
// rather than over mDNS, so without this projection the manual node's record
// would carry no engine view at all.

// TestManualNodeCarriesTheEngineNativeView pins that the engine's own view
// reaches both places a client reads a node from: the discovery store the
// discovery:get-nodes snapshot is taken from, and the relay directory the
// discovery:nodes-changed stream is taken from.
func TestManualNodeCarriesTheEngineNativeView(t *testing.T) {
	b := newManualTestBroker()
	const uuid = "remote-node-uuid"

	view := &enginenative.Stats{
		Engine:     enginenative.EngineOmlx,
		Supported:  true,
		Totals:     enginenative.Totals{Requests: 2, PromptTokens: 123, CompletionTokens: 599},
		Rates:      enginenative.Rates{PrefillTokensPerSecond: 161.1, GenerationTokensPerSecond: 69.1},
		Models:     []enginenative.Model{{Model: "a-model", Loaded: true}},
		ObservedAt: 1792663511584,
	}
	status := manualNodeStatus{
		ID:           "Retirement-Plan",
		Address:      "100.107.205.77",
		HostUUID:     uuid,
		ClusterUUID:  uuid,
		NodeInfoUp:   true,
		NodeInfoPort: 14318,
		EngineNative: view,
	}

	b.upsertManualNode(status)

	// The client-facing snapshot (discovery:get-nodes) carries the view.
	snap := b.store.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("want one node in the discovery store, got %d", len(snap))
	}
	if snap[0].EngineNative == nil {
		t.Fatal("the discovery snapshot carries no engine-native view")
	}
	if !enginenative.SameView(view, snap[0].EngineNative) {
		t.Errorf("engine-native view = %+v, want %+v", snap[0].EngineNative, view)
	}

	// The relayed node record (discovery:nodes-changed) carries it too.
	nodes := b.relayDir.Snapshot("")
	if len(nodes) != 1 {
		t.Fatalf("want one node in the relay directory, got %d", len(nodes))
	}
	if nodes[0].EngineNative == nil {
		t.Fatal("the relayed node record carries no engine-native view")
	}
	if !enginenative.SameView(view, nodes[0].EngineNative) {
		t.Errorf("relayed engine-native view = %+v, want %+v", nodes[0].EngineNative, view)
	}
}

// TestManualNodeWithoutAnEngineNativeViewCarriesNone pins that a node with no
// view carries none: the field is absent, not empty.
func TestManualNodeWithoutAnEngineNativeViewCarriesNone(t *testing.T) {
	b := newManualTestBroker()
	b.upsertManualNode(manualStatus("Retirement-Plan", "100.107.205.77", "remote-node-uuid"))

	snap := b.store.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("want one node in the discovery store, got %d", len(snap))
	}
	if snap[0].EngineNative != nil {
		t.Fatalf("a node with no engine-native view carries one: %+v", snap[0].EngineNative)
	}
	nodes := b.relayDir.Snapshot("")
	if len(nodes) != 1 {
		t.Fatalf("want one node in the relay directory, got %d", len(nodes))
	}
	if nodes[0].EngineNative != nil {
		t.Fatalf("a relayed node with no engine-native view carries one: %+v", nodes[0].EngineNative)
	}
}

// TestManualNodeEngineNativeViewIsOnTheNodeRecord pins the field the client
// reads: the node record the broker publishes for a manual node carries the
// same engineNative field a discovered node's record carries.
func TestManualNodeEngineNativeViewIsOnTheNodeRecord(t *testing.T) {
	b := newManualTestBroker()
	view := &enginenative.Stats{Engine: enginenative.EngineOmlx, Supported: true}
	status := manualNodeStatus{
		ID:           "Retirement-Plan",
		Address:      "100.107.205.77",
		HostUUID:     "remote-node-uuid",
		ClusterUUID:  "remote-node-uuid",
		NodeInfoUp:   true,
		NodeInfoPort: 14318,
		EngineNative: view,
	}
	node := b.manualToDirectoryNode(status, "remote-node-uuid")
	if node.EngineNative == nil {
		t.Fatal("the manual node's record carries no engine-native view")
	}
	if !enginenative.SameView(view, node.EngineNative) {
		t.Errorf("engine-native view = %+v, want %+v", node.EngineNative, view)
	}
}
