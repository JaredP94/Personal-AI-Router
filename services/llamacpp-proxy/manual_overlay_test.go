// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/noderec"
	"path/filepath"
	"testing"
)

// A broker-proven peer uses its manually configured overlay hostname, not a
// LAN address from discovery, and still requires a live exact certificate pin.
func TestManualOverlayHostnameKeepsPinnedTransport(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cluster")
	clustertrusttest.Join(t, dir, "cluster", "self", "peer")
	n, ok := subscribedToNode(noderec.DirectoryNode{
		HostUUID: "peer", ClusterUUID: "peer", Name: "peer", IP: "Workstation.tail.example",
		IPs:      []string{"Workstation.tail.example"},
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceLlamaCPP: {Port: 1234}},
	})
	if !ok {
		t.Fatal("manual address lost in relay projection")
	}
	disc := NewDiscovery()
	disc.SetSubscribed([]Node{n})
	p := testProxy(disc, 1235)
	p.mesh = clustertrust.Open(dir)
	got := p.resolveCandidates("")
	if len(got) != 1 || got[0].url.Scheme != "https" || got[0].url.Host != "Workstation.tail.example:1234" || got[0].peerUUID != "peer" {
		t.Fatalf("overlay candidates = %+v", got)
	}
	clustertrusttest.RemovePeerPin(t, dir, "peer")
	if got := p.resolveCandidates(""); len(got) != 0 {
		t.Fatalf("removed peer still routable: %+v", got)
	}
}
