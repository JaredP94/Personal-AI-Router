// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net"
	"net/http"
	"nvpair-shared/clustertrust"
	"nvpair-shared/enginenative"
	"nvpair-shared/noderec"
	"sort"
	"strconv"
)

func (m *Manager) inventoryClient(principal string) (*http.Client, bool) {
	m.mesh.Refresh()
	m.inventoryPoolOnce.Do(func() { m.inventoryPool = clustertrust.NewPeerClientPool(m.mesh, probeTimeout) })
	return m.inventoryPool.Client(principal)
}

func (m *Manager) authenticatedNodeInfo(ctx context.Context, entry ManualEntry, expected string) (NodeInfoResponse, int, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	port := 14318
	if entry.TLSPort > 0 {
		port = entry.TLSPort
	}
	if m.mesh == nil {
		return NodeInfoResponse{}, port, false
	}
	m.mesh.Refresh()
	url := "https://" + net.JoinHostPort(entry.Address, strconv.Itoa(port)) + "/v1/node-info"
	if expected == "" {
		cfg, ok := m.mesh.ClientTLSConfigAny()
		if !ok {
			return NodeInfoResponse{}, port, false
		}
		tr := &http.Transport{TLSClientConfig: cfg, DisableKeepAlives: true}
		defer tr.CloseIdleConnections()
		var hint NodeInfoResponse
		principal, _, ok := clustertrust.ReadPeerInventory(ctx, m.mesh, &http.Client{Transport: tr, Timeout: probeTimeout}, "", url, m.pollIntent(), nodeInfoResponseLimit, &hint)
		if !ok {
			return NodeInfoResponse{}, port, false
		}
		expected = principal
	}
	client, ok := m.inventoryClient(expected)
	if !ok {
		return NodeInfoResponse{}, port, false
	}
	var info NodeInfoResponse
	principal, binding, ok := clustertrust.ReadPeerInventory(ctx, m.mesh, client, expected, url, m.pollIntent(), nodeInfoResponseLimit, &info)
	if !ok || info.HostUUID != principal || info.ClusterUUID == nil || *info.ClusterUUID != principal || info.PeerMetadata == nil || info.PeerMetadata.HostUUID != principal || info.PeerMetadata.Services[noderec.ServiceNodeInfo] != port {
		return NodeInfoResponse{}, port, false
	}
	info.PeerRecordID = binding
	return info, port, true
}

func (m *Manager) probePeerEngineManager(ctx context.Context, addr, principal, binding string, port int, wantNative bool) (bool, engineManagerModels, *enginenative.Stats) {
	if port <= 0 || binding == "" {
		return false, engineManagerModels{}, nil
	}
	client, ok := m.inventoryClient(principal)
	if !ok || m.mesh.PeerRecordID(principal) != binding {
		return false, engineManagerModels{}, nil
	}
	url := "https://" + net.JoinHostPort(addr, strconv.Itoa(port))
	var models engineManagerModels
	_, got, ok := clustertrust.ReadPeerInventory(ctx, m.mesh, client, principal, url+"/v1/models", "", modelsResponseLimit, &models)
	if !ok || got != binding {
		return false, engineManagerModels{}, nil
	}
	if len(models.Models) == 0 && len(models.ModelsByEngine) > 0 {
		for _, inventory := range models.ModelsByEngine {
			models.Models = mergeModels(models.Models, inventory)
		}
		sort.Strings(models.Models)
	}
	var native *enginenative.Stats
	if wantNative {
		var stats enginenative.Stats
		_, got, ok := clustertrust.ReadPeerInventory(ctx, m.mesh, client, principal, url+"/v1/engine-stats", "", engineNativeResponseLimit, &stats)
		if ok && got == binding && stats.Supported {
			if stats.ObservedAt == 0 {
				stats.ObservedAt = m.clockNow().UnixMilli()
			}
			native = &stats
		}
	}
	if m.mesh.PeerRecordID(principal) != binding {
		return false, engineManagerModels{}, nil
	}
	return true, models, native
}
