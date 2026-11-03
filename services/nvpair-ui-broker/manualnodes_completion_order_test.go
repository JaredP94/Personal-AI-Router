// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestManualRemovalHintsExpireWithoutGrantingAuthority(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manual-nodes.json")
	if err := writePersistedManualNodes(path, []persistedManualNode{{ID: "alias", Kind: "pair", Address: "overlay-0.example"}}); err != nil {
		t.Fatal(err)
	}
	b := &Broker{clusterDir: dir, manualNodesConfigPath: path, store: newDiscoveryStore()}
	b.beginManualRemoval(manualClusterMember{NodeUUID: "peer", ClusterID: "cluster-1", AdmissionEpoch: 1})
	for i := 0; i < 2*manualRemovalHintLimit; i++ {
		params, _ := json.Marshal(manualPairingCompletion{Address: fmt.Sprintf("overlay-%d.example", i), PeerUUID: "peer", ClusterID: "cluster-1", AdmissionEpoch: 1})
		if err := b.associatePairedManualNodes(params); err != nil {
			t.Fatal(err)
		}
	}
	if hints := b.finishManualRemoval(false); len(hints) != manualRemovalHintLimit {
		t.Fatalf("unbounded cleanup hints: %d", len(hints))
	}
	if b.manualPendingRemoval != nil {
		t.Fatal("cleanup hints outlived the unpair")
	}
	stored, err := loadPersistedManualNodes(path)
	if err != nil || len(stored) != 1 || stored[0].PeerUUID != "" || len(b.schedulerNodes()) != 0 {
		t.Fatalf("transient hint became persistent association or authority: %+v %v", stored, err)
	}
}

func TestRemovedAdmissionRejectsCompletionDespiteStaleRoster(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manual-nodes.json")
	if err := writePersistedManualNodes(path, []persistedManualNode{{ID: "alias", Kind: "pair", Address: "overlay.example"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "members.json"), []byte(`[{"nodeUuid":"peer","clusterId":"cluster-1","admissionEpoch":1,"state":"member"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	b := &Broker{clusterDir: dir, manualNodesConfigPath: path}
	b.beginManualRemoval(manualClusterMember{NodeUUID: "peer", ClusterID: "cluster-1", AdmissionEpoch: 1})
	b.finishManualRemoval(true)
	if err := b.associatePairedManualNodes(json.RawMessage(`{"address":"overlay.example","peerUuid":"peer","clusterId":"cluster-1","admissionEpoch":1}`)); err != nil {
		t.Fatal(err)
	}
	stored, err := loadPersistedManualNodes(path)
	if err != nil || len(stored) != 1 || stored[0].PeerUUID != "" {
		t.Fatalf("completion after successful removal was accepted: %+v %v", stored, err)
	}
}

func TestObsoletePairingCompletionCannotAssociateManualEntry(t *testing.T) {
	for _, roster := range []string{
		`[]`,
		`[{"nodeUuid":"peer","clusterId":"cluster-1","admissionEpoch":2,"state":"member"}]`,
		`[{"nodeUuid":"peer","clusterId":"other-cluster","admissionEpoch":1,"state":"member"}]`,
		`[{"nodeUuid":"peer","clusterId":"cluster-1","admissionEpoch":1,"state":"pending"}]`,
	} {
		t.Run(roster, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "manual-nodes.json")
			if err := writePersistedManualNodes(path, []persistedManualNode{{Kind: "pair", ID: "alias", Address: "overlay.example"}}); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "members.json"), []byte(roster), 0600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			b := &Broker{codec: NewCodec(&out), clusterDir: dir, manualNodesConfigPath: path}
			b.forwardClusterManagerNotification("cluster:pairing-completed", json.RawMessage(`{"address":"overlay.example","peerUuid":"peer","clusterId":"cluster-1","admissionEpoch":1}`))
			stored, err := loadPersistedManualNodes(path)
			if err != nil || len(stored) != 1 || stored[0].PeerUUID != "" {
				t.Fatalf("obsolete completion persisted after removal/admission change: %+v %v", stored, err)
			}
		})
	}
}
