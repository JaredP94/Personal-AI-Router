// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestPairingCompletionDuringUnpairForgetsHostname(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manual-nodes.json")
	if err := writePersistedManualNodes(path, []persistedManualNode{{Kind: "pair", ID: "alias", Address: "Workstation.tail.example"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "members.json"), []byte(`[{"nodeUuid":"peer","ipAddress":"192.0.2.50"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	clientBroker, client := net.Pipe()
	defer clientBroker.Close()
	defer client.Close()
	clusterBroker, cluster := net.Pipe()
	defer clusterBroker.Close()
	defer cluster.Close()
	b := &Broker{codec: NewCodec(clientBroker), clusterDir: dir, manualNodesConfigPath: path}
	cm := &clusterManagerProcess{peer: NewPeer(NewCodec(clusterBroker)), onNotify: b.forwardClusterManagerNotification}
	b.setClusterMgr(cm)
	go cm.peer.Serve(nil, cm.handleNotify)
	go func() {
		codec := NewCodec(cluster)
		msg, err := codec.Read()
		if err != nil {
			return
		}
		_ = codec.Notify("cluster:pairing-completed", map[string]string{"address": "Workstation.tail.example", "peerUuid": "peer"})
		_ = codec.Respond(msg.ID, map[string]bool{"removed": true})
	}()
	id := json.RawMessage("1")
	go b.relayToClusterManager(&Message{JSONRPC: "2.0", ID: &id, Method: "nodes:remove", Params: json.RawMessage(`{"nodeUuid":"peer"}`)})
	response, err := NewCodec(client).Read()
	if err != nil || response.Error != nil {
		t.Fatalf("unpair response: %+v %v", response, err)
	}
	stored, err := loadPersistedManualNodes(path)
	if err != nil || len(stored) != 0 {
		t.Fatalf("in-flight association survived unpair: %+v %v", stored, err)
	}
}

func TestPairingCompletionForgetsHostnameBeforeInventoryProof(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manual-nodes.json")
	if err := writePersistedManualNodes(path, []persistedManualNode{{Kind: "pair", ID: "original-alias", Address: "Workstation.tail.example"}, {Kind: "engine", ID: "independent-engine", Address: "Workstation.tail.example"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "members.json"), []byte(`[{"id":"workstation","nodeUuid":"peer","ipAddress":"192.0.2.50"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	var frontend bytes.Buffer
	b := &Broker{codec: NewCodec(&frontend), clusterDir: dir, manualNodesConfigPath: path, store: newDiscoveryStore()}
	// This is a worker-owned post-authentication completion, not a public claim.
	b.forwardClusterManagerNotification("cluster:pairing-completed", json.RawMessage(`{"address":"Workstation.tail.example","peerUuid":"peer"}`))
	stored, err := loadPersistedManualNodes(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range stored {
		if entry.ID == "original-alias" && entry.PeerUUID != "peer" {
			t.Fatalf("pairing did not persist hostname-to-peer cleanup association: %+v", entry)
		}
		if entry.Kind == "engine" && entry.PeerUUID != "" {
			t.Fatal("pairing attached a bare engine to peer lifecycle")
		}
	}
	if frontend.Len() != 0 {
		t.Fatal("internal cleanup association leaked to renderer")
	}
	if len(b.schedulerNodes()) != 0 {
		t.Fatal("cleanup association granted inventory or routing authority")
	}

	// Restart the broker before any node-info proof. Only durable configuration
	// and a numeric roster address remain; unpair must still forget the alias.
	clientBroker, client := net.Pipe()
	defer clientBroker.Close()
	defer client.Close()
	clusterBroker, cluster := net.Pipe()
	defer clusterBroker.Close()
	defer cluster.Close()
	cm := &clusterManagerProcess{peer: NewPeer(NewCodec(clusterBroker))}
	go cm.peer.Serve(nil, nil)
	go func() {
		codec := NewCodec(cluster)
		msg, err := codec.Read()
		if err == nil {
			_ = codec.Respond(msg.ID, map[string]bool{"removed": true})
		}
	}()
	restarted := &Broker{codec: NewCodec(clientBroker), clusterDir: dir, manualNodesConfigPath: path}
	restarted.setClusterMgr(cm)
	id := json.RawMessage("1")
	go restarted.relayToClusterManager(&Message{JSONRPC: "2.0", ID: &id, Method: "nodes:remove", Params: json.RawMessage(`{"nodeUuid":"peer"}`)})
	response, err := NewCodec(client).Read()
	if err != nil || response.Error != nil {
		t.Fatalf("unpair response: %+v, %v", response, err)
	}
	stored, err = loadPersistedManualNodes(path)
	if err != nil || len(stored) != 1 || stored[0].Kind != "engine" {
		t.Fatalf("hostname survived unpair and can replay: %+v, %v", stored, err)
	}
}
