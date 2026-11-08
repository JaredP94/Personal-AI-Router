// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net"
	"nvpair-shared/clustertrust"
	"nvpair-shared/noderec"
	"slices"
	"strconv"
	"time"
)

func (d *daemon) localNode(hostUUID string) bool {
	return d.reg != nil && hostUUID == d.reg.record().HostUUID
}

func (d *daemon) currentPeer(node noderec.DirectoryNode) bool {
	if d.localNode(node.HostUUID) {
		return true
	}
	d.mesh.Refresh()
	return node.ServicePeer() && node.PeerRecordID == d.mesh.PeerRecordID(node.ClusterUUID)
}

func (d *daemon) authenticatedInfo(ctx context.Context, node noderec.DirectoryNode, hosts []string, port int) (NodeInfoResponse, string, bool) {
	if node.HostUUID == "" || node.ClusterUUID == "" || node.HostUUID != node.ClusterUUID {
		return NodeInfoResponse{}, "", false
	}
	d.mesh.Refresh()
	d.peerInfoOnce.Do(func() { d.peerInfoClients = clustertrust.NewPeerClientPool(d.mesh, nodeInfoFetchTimeout) })
	client, ok := d.peerInfoClients.Client(node.ClusterUUID)
	if !ok {
		return NodeInfoResponse{}, "", false
	}
	hosts = d.candidateHosts(node, hosts, time.Now())
	for _, host := range hosts {
		var info NodeInfoResponse
		url := "https://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/v1/node-info"
		principal, binding, ok := clustertrust.ReadPeerInventory(ctx, d.mesh, client, node.ClusterUUID, url, d.pollIntent(), nodeInfoResponseLimit, &info)
		if !ok || info.HostUUID != principal || info.ClusterUUID == nil || *info.ClusterUUID != principal || info.PeerMetadata == nil || info.PeerMetadata.HostUUID != principal || info.PeerMetadata.Name == "" || info.PeerMetadata.Services[noderec.ServiceNodeInfo] != port {
			continue
		}
		info.PeerRecordID = binding
		return info, host, true
	}
	return NodeInfoResponse{}, "", false
}

func applyAuthenticatedInfo(node *noderec.DirectoryNode, info NodeInfoResponse, host string) {
	meta := info.PeerMetadata
	node.Trusted = true
	node.IdentityVerification = "verified"
	node.PeerRecordID = info.PeerRecordID
	node.Name = meta.Name
	// Only the endpoint which actually proved its identity is promoted. Other
	// mDNS addresses remain hints for a later authentication, never routing data.
	node.IP = host
	node.IPs = []string{host}
	node.Services = make(map[noderec.ServiceKey]noderec.ServiceStatus, len(meta.Services))
	for service, port := range meta.Services {
		if port > 0 && port <= 65535 {
			node.Services[service] = noderec.ServiceStatus{Port: port}
		}
	}
	node.GPUs, node.CPU, node.Memory = info.GPUs, info.CPU, info.Memory
	node.LastSeen = time.Now().Unix()
}

func (d *daemon) candidateSnapshot() []noderec.DirectoryNode {
	d.candidateMu.Lock()
	defer d.candidateMu.Unlock()
	out := make([]noderec.DirectoryNode, 0, len(d.candidates))
	for _, node := range d.candidates {
		out = append(out, node)
	}
	return out
}

func (d *daemon) publishVerified(node noderec.DirectoryNode, candidateKey string) bool {
	d.inventoryMu.Lock()
	defer d.inventoryMu.Unlock()
	if !d.currentPeer(node) {
		return false
	}
	if candidateKey != "" {
		d.candidateMu.Lock()
		hint, exists := d.candidates[candidateKey]
		d.candidateMu.Unlock()
		if !exists || hint.HostUUID != node.HostUUID || !slices.Contains(hint.CandidateIPs(), node.IP) {
			return false
		}
	}
	// Retire the promoting preview even if the proven inventory is unchanged.
	// Other revisions of this advertiser were retired on browse. Never match
	// previews by claimed UUID alone: another instance/host is independent.
	if candidateKey != "" {
		if preview, exists := d.dir.get(candidateKey); exists {
			d.dir.remove(candidateKey)
			d.emit(noderec.NotifyNodeRemoved, preview)
		}
	}
	if prior, exists := d.dir.get(node.HostUUID); exists && prior.PeerRecordID == node.PeerRecordID {
		if node.Models == nil {
			node.Models, node.ModelsByEngine, node.LoadedByEngine = prior.Models, prior.ModelsByEngine, prior.LoadedByEngine
		}
		if node.EngineNative == nil {
			node.EngineNative = prior.EngineNative
		}
		if sameIgnoringLastSeen(prior, node) {
			d.dir.replaceIfCurrent(prior, node)
			return false
		}
	} else if exists {
		d.forget(node.HostUUID)
	}
	d.dir.upsert(node)
	d.emit(noderec.NotifyNodeUpdated, node)
	return true
}

func (d *daemon) expireVerified(node noderec.DirectoryNode) {
	d.inventoryMu.Lock()
	defer d.inventoryMu.Unlock()
	if !d.dir.removeIfCurrent(node) {
		return
	}
	d.forget(node.HostUUID)
	d.emit(noderec.NotifyNodeRemoved, node)
}

func withdrawPeer(node *noderec.DirectoryNode) {
	node.Trusted = false
	node.PeerRecordID = ""
	node.IdentityVerification = "unverified"
	node.GPUs, node.CPU, node.Memory = nil, nil, nil
	node.Models, node.ModelsByEngine, node.LoadedByEngine = nil, nil, nil
	node.EngineNative = nil
}
