// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const manualRemovalHintLimit = 16

type manualClusterMember struct {
	ID             string `json:"id"`
	NodeUUID       string `json:"nodeUuid"`
	Address        string `json:"ipAddress"`
	ClusterID      string `json:"clusterId"`
	AdmissionEpoch uint64 `json:"admissionEpoch"`
	State          string `json:"state"`
}

type manualPairingCompletion struct {
	Address        string `json:"address"`
	PeerUUID       string `json:"peerUuid"`
	ClusterID      string `json:"clusterId"`
	AdmissionEpoch uint64 `json:"admissionEpoch"`
}

type pendingManualRemoval struct {
	member    manualClusterMember
	addresses map[string]bool
}

func (b *Broker) manualMemberForID(id string) (manualClusterMember, error) {
	member := manualClusterMember{NodeUUID: id}
	if id == "" || b.clusterDir == "" {
		return member, nil
	}
	data, err := os.ReadFile(filepath.Join(b.clusterDir, "members.json"))
	if errors.Is(err, os.ErrNotExist) {
		return member, nil
	}
	if err != nil {
		return member, err
	}
	var members []manualClusterMember
	if err := json.Unmarshal(data, &members); err != nil {
		return member, err
	}
	for _, candidate := range members {
		if candidate.NodeUUID != "" && (candidate.NodeUUID == id || candidate.ID == id) {
			return candidate, nil
		}
	}
	return member, nil
}

// manualMutationMu serializes these unpair transactions. The notification reader
// never takes that lock, so it can consume a completion while Call awaits removal.
func (b *Broker) beginManualRemoval(member manualClusterMember) {
	b.manualCleanupMu.Lock()
	defer b.manualCleanupMu.Unlock()
	b.manualPendingRemoval = &pendingManualRemoval{member: member, addresses: make(map[string]bool)}
}

// Hints live only until this unpair ends (success or failure), have a fixed size
// cap, are never persisted, and are consumed only for configuration deletion.
func (b *Broker) finishManualRemoval(removed bool) []string {
	b.manualCleanupMu.Lock()
	defer b.manualCleanupMu.Unlock()
	pending := b.manualPendingRemoval
	b.manualPendingRemoval = nil
	if pending == nil {
		return nil
	}
	if removed && pending.member.ClusterID != "" && pending.member.AdmissionEpoch > 0 {
		if b.manualRemovedAdmissions == nil {
			b.manualRemovedAdmissions = make(map[string]uint64)
		}
		key := pending.member.ClusterID + "\x00" + pending.member.NodeUUID
		if pending.member.AdmissionEpoch > b.manualRemovedAdmissions[key] {
			b.manualRemovedAdmissions[key] = pending.member.AdmissionEpoch
		}
	}
	addresses := make([]string, 0, len(pending.addresses))
	for address := range pending.addresses {
		addresses = append(addresses, address)
	}
	return addresses
}

// Only the supervised cluster-manager's authenticated commit emits this event.
// Current roster/admission validation prevents obsolete completions from
// persisting an association after removal or readmission. No service authority
// is inferred, and there is no worker call or mutation-lock wait on this reader.
func (b *Broker) associatePairedManualNodes(params json.RawMessage) error {
	var paired manualPairingCompletion
	if err := json.Unmarshal(params, &paired); err != nil {
		return err
	}
	if paired.Address == "" || paired.PeerUUID == "" || paired.ClusterID == "" || paired.AdmissionEpoch == 0 {
		return fmt.Errorf("invalid pairing cleanup association")
	}
	b.manualCleanupMu.Lock()
	defer b.manualCleanupMu.Unlock()
	key := paired.ClusterID + "\x00" + paired.PeerUUID
	if b.manualRemovedAdmissions[key] >= paired.AdmissionEpoch {
		return nil
	}
	if pending := b.manualPendingRemoval; pending != nil && pending.member.NodeUUID == paired.PeerUUID &&
		(pending.member.ClusterID == "" || pending.member.ClusterID == paired.ClusterID) &&
		(pending.member.AdmissionEpoch == 0 || pending.member.AdmissionEpoch == paired.AdmissionEpoch) {
		pending.member.ClusterID = paired.ClusterID
		pending.member.AdmissionEpoch = paired.AdmissionEpoch
		if len(pending.addresses) < manualRemovalHintLimit {
			pending.addresses[paired.Address] = true
		}
	}
	member, err := b.manualMemberForID(paired.PeerUUID)
	if err != nil {
		return err
	}
	if member.State != "member" || member.ClusterID != paired.ClusterID || member.AdmissionEpoch != paired.AdmissionEpoch {
		return nil
	}
	b.manualPersistMu.Lock()
	defer b.manualPersistMu.Unlock()
	entries, err := loadPersistedManualNodes(b.manualNodesConfigPath)
	if err != nil {
		return err
	}
	changed := false
	for i := range entries {
		if entries[i].Kind == "pair" && entries[i].Address == paired.Address && entries[i].PeerUUID != paired.PeerUUID {
			entries[i].PeerUUID = paired.PeerUUID
			changed = true
		}
	}
	if changed {
		return writePersistedManualNodes(b.manualNodesConfigPath, entries)
	}
	return nil
}
