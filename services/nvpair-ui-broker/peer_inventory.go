// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"nvpair-shared/noderec"
	"nvpair-ui-broker/relay"
	"os"
	"sort"
)

func (b *Broker) pushPeerMetadataToNodeInfo() {
	np := b.getNodeInfo()
	if np == nil || b.regCache == nil {
		return
	}
	// Serialize snapshot creation with writes so concurrent registrations cannot
	// publish an older snapshot after a newer one.
	np.stdinMu.Lock()
	defer np.stdinMu.Unlock()
	name, _ := os.Hostname()
	meta := noderec.PeerMetadataParams{HostUUID: b.nodeID, Name: name, Services: make(map[noderec.ServiceKey]int)}
	for _, p := range b.regCache.Snapshot() {
		meta.Services[p.Service] = p.Port
	}
	if err := np.codec.Notify(noderec.MethodSetPeerMetadata, meta); err != nil {
		slog.Debug("failed to publish local peer metadata", "err", err)
	}
}

func (b *Broker) normalizeManualStatus(s manualNodeStatus) manualNodeStatus {
	if s.PeerRecordID == "" {
		return s
	}
	if s.HostUUID == s.ClusterUUID && s.PeerMetadata != nil && s.PeerMetadata.HostUUID == s.HostUUID && b.currentPeerProof(s.ClusterUUID, s.PeerRecordID) {
		return s
	}
	// A retired worker proof may remain as an unverified configuration preview,
	// but never under the peer's host UUID and never as a bare-engine bypass.
	s.PeerRecordID = ""
	s.IdentityVerification = "unverified"
	s.PeerMetadata = nil
	s.NodeInfoUp = false
	s.OllamaUp = false
	s.LMStudioUp = false
	s.OMLXUp = false
	s.Models, s.OllamaModels, s.LMStudioModels, s.OMLXModels = nil, nil, nil, nil
	s.ModelsByEngine, s.LoadedByEngine = nil, nil
	s.GPUs, s.CPU, s.Memory = nil, nil, nil
	s.EngineNative = nil
	s.TelemetryValid = false
	return s
}

func (b *Broker) authorizedDirectoryNode(n noderec.DirectoryNode) bool {
	if n.IdentityVerification == "local" {
		return n.HostUUID != "" && n.HostUUID == b.nodeID
	}
	return n.ServicePeer() && n.HostUUID == n.ClusterUUID && b.currentPeerProof(n.ClusterUUID, n.PeerRecordID)
}

func (b *Broker) decisionInventory(n EnrichedNode) bool {
	if n.IdentityVerification == "local" {
		return n.HostUUID != "" && n.HostUUID == b.nodeID
	}
	if n.ManualEngine {
		return true
	}
	return n.IdentityVerification == "verified" && n.Trusted && n.HostUUID == n.ClusterUUID && b.currentPeerProof(n.ClusterUUID, n.PeerRecordID)
}

// schedulerNodes never uses the public preview projection as a node universe.
func (b *Broker) schedulerNodes() []AvailableNode {
	if b.store == nil {
		return nil
	}
	b.store.mu.RLock()
	records := make([]storedNode, 0, len(b.store.nodes))
	for _, sn := range b.store.nodes {
		records = append(records, sn)
	}
	b.store.mu.RUnlock()
	out := make([]AvailableNode, 0, len(records))
	for _, sn := range records {
		if sn.scanner != nil && !b.decisionInventory(*sn.scanner) {
			sn.scanner = nil
		}
		if sn.manual != nil && !b.decisionInventory(*sn.manual) {
			sn.manual = nil
		}
		if sn.claimed() {
			out = append(out, sn.toAvailable())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].HostUUID < out[j].HostUUID })
	return out
}

func (b *Broker) hasDecisionInventory(hostUUID string) bool {
	if b.store == nil {
		return false
	}
	b.store.mu.RLock()
	sn := b.store.nodes[hostUUID]
	b.store.mu.RUnlock()
	return (sn.scanner != nil && b.decisionInventory(*sn.scanner)) || (sn.manual != nil && b.decisionInventory(*sn.manual))
}

func (b *Broker) authorizedTelemetry(source nodeSource, hostUUID, proof string) bool {
	if b.store == nil {
		return false
	}
	b.store.mu.RLock()
	sn := b.store.nodes[hostUUID]
	var n *EnrichedNode
	if source == sourceScanner {
		n = sn.scanner
	} else {
		n = sn.manual
	}
	b.store.mu.RUnlock()
	if n == nil || n.ManualEngine {
		return false
	}
	if n.IdentityVerification == "local" {
		return hostUUID != "" && hostUUID == b.nodeID
	}
	return proof != "" && n.PeerRecordID == proof && b.decisionInventory(*n)
}

// Withdraw each stale source independently; an authorized source from the other
// worker must survive. Rechecking every incoming proof also rejects late frames.
func (b *Broker) withdrawUnauthorizedInventory() {
	if b.store == nil {
		return
	}
	type stale struct {
		node   EnrichedNode
		source nodeSource
	}
	var staleNodes []stale
	b.store.mu.RLock()
	for _, sn := range b.store.nodes {
		if sn.scanner != nil && sn.scanner.IdentityVerification == "verified" && !b.decisionInventory(*sn.scanner) {
			staleNodes = append(staleNodes, stale{*sn.scanner, sourceScanner})
		}
		if sn.manual != nil && sn.manual.IdentityVerification == "verified" && !b.decisionInventory(*sn.manual) {
			staleNodes = append(staleNodes, stale{*sn.manual, sourceManual})
		}
	}
	b.store.mu.RUnlock()
	for _, entry := range staleNodes {
		n := entry.node
		if !b.store.withdrawProof(entry.source, entry.node.PeerRecordID, n) {
			continue
		}
		b.removeTelemetry(entry.source, n.HostUUID)
		if b.relayDir != nil {
			source := relay.SourceScanner
			if entry.source == sourceManual {
				source = relay.SourceManual
			}
			b.relayDir.ApplySource(source, noderec.NotifyNodeRemoved, noderec.DirectoryNode{HostUUID: n.HostUUID, PeerRecordID: entry.node.PeerRecordID, IdentityVerification: "verified"})
		}
		b.store.mu.RLock()
		current := b.store.nodes[n.HostUUID]
		b.store.mu.RUnlock()
		if b.workloads != nil && (current.scanner == nil || !b.decisionInventory(*current.scanner)) && (current.manual == nil || !b.decisionInventory(*current.manual)) {
			b.failWorkloadsForNode(n.HostUUID, n.ID)
		}
	}
}

func (s *discoveryStore) withdrawProof(source nodeSource, proof string, replacement EnrichedNode) bool {
	s.mu.Lock()
	sn := s.nodes[replacement.HostUUID]
	prior := sn.scanner
	if source == sourceManual {
		prior = sn.manual
	}
	if prior == nil || prior.PeerRecordID != proof {
		s.mu.Unlock()
		return false
	}
	sn.setSource(source, nil)
	if sn.claimed() {
		s.nodes[replacement.HostUUID] = sn
	} else {
		delete(s.nodes, replacement.HostUUID)
	}
	onChange := s.onChange
	s.mu.Unlock()
	if onChange != nil {
		onChange()
	}
	return true
}

func (b *Broker) clearScannerInventory() {
	sp := b.getScanner()
	if sp != nil {
		sp.notifyMu.Lock()
		defer sp.notifyMu.Unlock()
	}
	b.scannerInventoryGeneration.Add(1)
	b.setScanner(nil)
	if b.store == nil {
		return
	}
	b.store.mu.RLock()
	var nodes []EnrichedNode
	for _, sn := range b.store.nodes {
		if sn.scanner != nil {
			nodes = append(nodes, *sn.scanner)
		}
	}
	b.store.mu.RUnlock()
	for _, n := range nodes {
		gone := b.store.Remove(n.storeKey(), sourceScanner)
		if b.relayDir != nil {
			b.relayDir.ApplySource(relay.SourceScanner, noderec.NotifyNodeRemoved, noderec.DirectoryNode{HostUUID: n.HostUUID, PeerRecordID: n.PeerRecordID, IdentityVerification: n.IdentityVerification})
		}
		if n.IdentityVerification == "verified" || n.IdentityVerification == "local" {
			b.removeTelemetry(sourceScanner, n.HostUUID)
		}
		if gone && b.workloads != nil && (n.IdentityVerification == "verified" || n.IdentityVerification == "local") {
			b.failWorkloadsForNode(n.HostUUID, n.ID)
		}
	}
}

func (b *Broker) currentPeerProof(principal, recordID string) bool {
	if b.mesh == nil || recordID == "" {
		return false
	}
	b.mesh.Refresh()
	return b.mesh.PeerRecordID(principal) == recordID
}
