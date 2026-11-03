// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVerifiedAssociationSurvivesOfflineReplayWithoutGrantingTrust(t *testing.T) {
	b := newManualTestBroker(t, "peer")
	b.manualNodesConfigPath = filepath.Join(t.TempDir(), "manual-nodes.json")
	entry := persistedManualNode{Kind: "pair", ID: "overlay", Address: "MagicDNS.example", Name: "overlay"}
	if err := b.persistManualNode(entry); err != nil {
		t.Fatal(err)
	}
	status := ownedManualStatus(t, b, manualNodeStatus{Kind: "pair", ID: "overlay", Address: entry.Address, HostUUID: "peer", IdentityVerification: "verified"})
	b.upsertManualNode(status)
	stored, err := loadPersistedManualNodes(b.manualNodesConfigPath)
	if err != nil || len(stored) != 1 || stored[0].PeerUUID != "peer" {
		t.Fatalf("proven association: %+v %v", stored, err)
	}
	b.manualNodeStatuses = map[string]manualNodeStatusEntry{}
	if err := b.persistManualNode(entry); err != nil {
		t.Fatal(err)
	}
	aliases, err := b.manualAliasesForPeer("peer")
	if err != nil || len(aliases) != 1 || aliases[0] != "overlay" {
		t.Fatalf("offline aliases: %v %v", aliases, err)
	}
	b.upsertManualNode(manualNodeStatus{Kind: "pair", ID: "overlay", Address: entry.Address, IdentityVerification: "unverified"})
	if got := b.schedulerNodes(); len(got) != 0 {
		t.Fatalf("stored association authorized routing: %+v", got)
	}
}

func TestUnpairMatchesRosterAddressBeforeInventoryProof(t *testing.T) {
	dir := t.TempDir()
	b := &Broker{clusterDir: dir, manualNodesConfigPath: filepath.Join(dir, "manual-nodes.json")}
	if err := os.WriteFile(filepath.Join(dir, "members.json"), []byte(`[{"id":"workstation","nodeUuid":"peer","ipAddress":"MagicDNS.example"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writePersistedManualNodes(b.manualNodesConfigPath, []persistedManualNode{{ID: "overlay", Address: "MagicDNS.example", Kind: "pair"}, {ID: "bare", Address: "MagicDNS.example", Kind: "engine"}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"peer", "workstation"} {
		aliases, err := b.manualAliasesForPeer(id)
		if err != nil || len(aliases) != 1 || aliases[0] != "overlay" {
			t.Fatalf("roster-only aliases for %q: %v %v", id, aliases, err)
		}
	}
}

func TestLegacyManualIntentMigratesToPairOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manual-nodes.json")
	if err := os.WriteFile(path, []byte(`[{"id":"old","address":"MagicDNS.example","name":"old"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := loadPersistedManualNodes(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Kind != "pair" || entries[0].Address != "MagicDNS.example" {
		t.Fatalf("migration = %+v", entries)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored []persistedManualNode
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Kind != "pair" {
		t.Fatalf("migration was not durable: %s", data)
	}
	if err := (&Broker{manualNodesConfigPath: path}).persistManualNode(persistedManualNode{ID: "engine", Address: "engine.example", Kind: "engine"}); err != nil {
		t.Fatal(err)
	}
	entries, err = loadPersistedManualNodes(path)
	if err != nil || len(entries) != 2 || entries[0].Kind != "engine" || entries[1].Kind != "pair" {
		t.Fatalf("explicit engine persistence: %+v, %v", entries, err)
	}
}

func TestPersistManualNodeRoundTripsAndRemoves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "configs", "manual-nodes.json")
	b := &Broker{manualNodesConfigPath: path}

	if err := b.persistManualNode(persistedManualNode{Kind: "pair", ID: "lab", Address: "node.local", Name: "lab", TLSPort: 14319, MTLS: true}); err != nil {
		t.Fatal(err)
	}
	entries, err := loadPersistedManualNodes(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Kind != "pair" || entries[0].ID != "lab" || entries[0].TLSPort != 14319 || !entries[0].MTLS {
		t.Fatalf("persisted entries = %+v", entries)
	}

	if err := b.removePersistedManualNode("lab"); err != nil {
		t.Fatal(err)
	}
	entries, err = loadPersistedManualNodes(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries after remove = %+v, want empty", entries)
	}
}

func TestReplayPersistedManualNodesIntoWorker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "configs", "manual-nodes.json")
	if err := writePersistedManualNodes(path, []persistedManualNode{{ID: "lab", Address: "node.local", Name: "lab"}}); err != nil {
		t.Fatal(err)
	}

	brokerSide, workerSide := net.Pipe()
	defer brokerSide.Close()
	defer workerSide.Close()
	worker := &rpcWorker{peer: NewPeer(NewCodec(brokerSide))}
	go worker.peer.Serve(nil, nil)

	got := make(chan persistedManualNode, 1)
	go func() {
		codec := NewCodec(workerSide)
		msg, err := codec.Read()
		if err != nil {
			return
		}
		var entry persistedManualNode
		if json.Unmarshal(msg.Params, &entry) == nil {
			got <- entry
		}
		_ = codec.Respond(msg.ID, map[string]string{"id": "lab"})
	}()

	b := &Broker{manualNodesConfigPath: path}
	b.replayPersistedManualNodes(worker)

	select {
	case entry := <-got:
		if entry.Kind != "pair" || entry.Address != "node.local" || entry.Name != "lab" {
			t.Fatalf("replayed entry = %+v", entry)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("persisted manual node was not replayed")
	}
}

func TestManualAliasForOperationalKey(t *testing.T) {
	b := &Broker{manualNodeKeys: map[string]string{"second": "node-uuid", "first": "node-uuid"}}
	if got := b.manualAliasForKey("node-uuid"); got != "first" {
		t.Fatalf("alias = %q, want first", got)
	}
	if got := b.manualAliasForKey("second"); got != "second" {
		t.Fatalf("direct alias = %q, want second", got)
	}
}

func TestManualNodeRelayPersistsSuccessfulAdd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "configs", "manual-nodes.json")
	clientBroker, clientSide := net.Pipe()
	workerBroker, workerSide := net.Pipe()
	defer clientBroker.Close()
	defer clientSide.Close()
	defer workerBroker.Close()
	defer workerSide.Close()

	worker := &rpcWorker{peer: NewPeer(NewCodec(workerBroker))}
	go worker.peer.Serve(nil, nil)
	go func() {
		codec := NewCodec(workerSide)
		msg, err := codec.Read()
		if err != nil {
			return
		}
		_ = codec.Respond(msg.ID, map[string]any{"id": "manual:node.local", "address": "node.local"})
	}()

	b := &Broker{codec: NewCodec(clientBroker), manualNodesConfigPath: path}
	b.setManualNodes(worker)
	id := json.RawMessage("1")
	b.relayToManualNodes(&Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "node/add",
		Params:  json.RawMessage(`{"address":"node.local"}`),
	})
	response, err := NewCodec(clientSide).Read()
	if err != nil || response.Error != nil {
		t.Fatalf("add response error = %v, frame = %+v", err, response)
	}

	entries, err := loadPersistedManualNodes(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != "manual:node.local" || entries[0].Address != "node.local" {
		t.Fatalf("persisted add = %+v", entries)
	}
}

func TestManualNodeRelayRemovesByOperationalKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "configs", "manual-nodes.json")
	if err := writePersistedManualNodes(path, []persistedManualNode{{ID: "lab", Address: "node.local", Name: "lab"}}); err != nil {
		t.Fatal(err)
	}
	clientBroker, clientSide := net.Pipe()
	workerBroker, workerSide := net.Pipe()
	defer clientBroker.Close()
	defer clientSide.Close()
	defer workerBroker.Close()
	defer workerSide.Close()

	worker := &rpcWorker{peer: NewPeer(NewCodec(workerBroker))}
	go worker.peer.Serve(nil, nil)
	seenID := make(chan string, 1)
	go func() {
		codec := NewCodec(workerSide)
		msg, err := codec.Read()
		if err != nil {
			return
		}
		var params struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(msg.Params, &params)
		seenID <- params.ID
		_ = codec.Respond(msg.ID, map[string]bool{"removed": true})
	}()

	b := &Broker{
		codec:                 NewCodec(clientBroker),
		manualNodesConfigPath: path,
		manualNodeKeys:        map[string]string{"lab": "node-uuid"},
	}
	b.setManualNodes(worker)
	id := json.RawMessage("2")
	b.relayToManualNodes(&Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "node/remove",
		Params:  json.RawMessage(`{"id":"node-uuid"}`),
	})
	response, err := NewCodec(clientSide).Read()
	if err != nil || response.Error != nil {
		t.Fatalf("remove response error = %v, frame = %+v", err, response)
	}
	if got := <-seenID; got != "lab" {
		t.Fatalf("worker remove id = %q, want lab", got)
	}
	entries, err := loadPersistedManualNodes(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("persisted entries after remove = %+v", entries)
	}
}
