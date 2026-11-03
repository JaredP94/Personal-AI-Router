// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestCurrentManualAssociationSurvivesStaleObservationAndReadd(t *testing.T) {
	b := newManualTestBroker(t, "peer-a", "peer-b")
	b.clusterDir = t.TempDir()
	b.manualNodesConfigPath = filepath.Join(b.clusterDir, "manual-nodes.json")
	entry := persistedManualNode{Kind: "pair", ID: "alias", Name: "alias", Address: "Workstation.tail.example", PeerUUID: "peer-a"}
	if err := writePersistedManualNodes(b.manualNodesConfigPath, []persistedManualNode{entry}); err != nil {
		t.Fatal(err)
	}
	roster := `[{"nodeUuid":"peer-a","clusterId":"fixture-cluster","admissionEpoch":1,"state":"member"},{"nodeUuid":"peer-b","clusterId":"fixture-cluster","admissionEpoch":1,"state":"member"}]`
	if err := os.WriteFile(filepath.Join(b.clusterDir, "members.json"), []byte(roster), 0600); err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(manualPairingCompletion{Address: entry.Address, PeerUUID: "peer-b", ClusterID: "fixture-cluster", AdmissionEpoch: 1})
	if err := b.associatePairedManualNodes(params); err != nil {
		t.Fatal(err)
	}
	stale := ownedManualStatus(t, b, manualNodeStatus{Kind: "pair", IdentityVerification: "verified", ID: entry.ID, Address: entry.Address, HostUUID: "peer-a"})
	b.upsertManualNode(stale)
	b.refreshManualNodesTrust()
	entry.PeerUUID = "" // ordinary re-add cannot seed from the stale cached A proof
	if err := b.persistManualNode(entry); err != nil {
		t.Fatal(err)
	}
	stored, err := loadPersistedManualNodes(b.manualNodesConfigPath)
	if err != nil || len(stored) != 1 || stored[0].PeerUUID != "peer-b" {
		t.Fatalf("stale observation/re-add replaced current B association: %+v %v", stored, err)
	}
}

func TestUnpairAssociationOverridesCachedIdentityAndReusedAddress(t *testing.T) {
	for _, address := range []string{"Workstation.tail.example", "192.0.2.50"} {
		t.Run(address, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "manual-nodes.json")
			if err := writePersistedManualNodes(path, []persistedManualNode{{Kind: "pair", ID: "alias", Address: address, PeerUUID: "peer-b"}}); err != nil {
				t.Fatal(err)
			}
			roster, _ := json.Marshal([]manualClusterMember{{NodeUUID: "peer-a", Address: address, State: "member"}, {NodeUUID: "peer-b", Address: "192.0.2.99", State: "member"}})
			if err := os.WriteFile(filepath.Join(dir, "members.json"), roster, 0600); err != nil {
				t.Fatal(err)
			}
			b := &Broker{clusterDir: dir, manualNodesConfigPath: path, manualNodeStatuses: map[string]manualNodeStatusEntry{"alias": {status: manualNodeStatus{Kind: "pair", Address: address, HostUUID: "peer-a"}}}}
			aliases, err := b.manualAliasesForPeer("peer-a")
			if err != nil || len(aliases) != 0 {
				t.Fatalf("removing A selected B's alias using stale identity/reused address: %v %v", aliases, err)
			}
			aliases, err = b.manualAliasesForPeer("peer-b")
			if err != nil || len(aliases) != 1 || aliases[0] != "alias" {
				t.Fatalf("current B association did not own its alias: %v %v", aliases, err)
			}
		})
	}
}

func TestUnpairRechecksAssociationChangedDuringRequest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manual-nodes.json")
	if err := writePersistedManualNodes(path, []persistedManualNode{{Kind: "pair", ID: "alias", Address: "overlay.example", PeerUUID: "peer-a"}}); err != nil {
		t.Fatal(err)
	}
	roster := `[{"nodeUuid":"peer-a","clusterId":"cluster-1","admissionEpoch":1,"state":"member"},{"nodeUuid":"peer-b","clusterId":"cluster-1","admissionEpoch":1,"state":"member"}]`
	if err := os.WriteFile(filepath.Join(dir, "members.json"), []byte(roster), 0600); err != nil {
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
		_ = codec.Notify("cluster:pairing-completed", manualPairingCompletion{Address: "overlay.example", PeerUUID: "peer-b", ClusterID: "cluster-1", AdmissionEpoch: 1})
		_ = codec.Respond(msg.ID, map[string]bool{"removed": true})
	}()
	id := json.RawMessage("1")
	go b.relayToClusterManager(&Message{JSONRPC: "2.0", ID: &id, Method: "nodes:remove", Params: json.RawMessage(`{"nodeUuid":"peer-a"}`)})
	response, err := NewCodec(client).Read()
	if err != nil || response.Error != nil {
		t.Fatalf("unpair response: %+v %v", response, err)
	}
	stored, err := loadPersistedManualNodes(path)
	if err != nil || len(stored) != 1 || stored[0].PeerUUID != "peer-b" {
		t.Fatalf("pre-removal snapshot deleted newly reassociated B entry: %+v %v", stored, err)
	}
}
