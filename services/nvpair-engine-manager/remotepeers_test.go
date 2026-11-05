// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"slices"
	"testing"

	"nvpair-shared/noderec"
)

func TestPeerPreviewCannotShadowVerifiedDownloadTarget(t *testing.T) {
	verified := noderec.DirectoryNode{HostUUID: "same-claim", PeerRecordID: "verified-record", IdentityVerification: "verified", Trusted: true, IP: "192.0.2.1", ClusterUUID: "pinned", Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceEngineControl: {Port: 14323}}}
	preview := noderec.DirectoryNode{HostUUID: "same-claim", PeerRecordID: "preview:other", IdentityVerification: "unverified", IP: "192.0.2.2", ClusterUUID: "pinned", Services: verified.Services}
	for _, nodes := range [][]noderec.DirectoryNode{{verified, preview}, {preview, verified}} {
		d := newPeerDirectory()
		d.set(nodes)
		peer, ok := d.lookup("same-claim")
		if !ok || !peer.verified || peer.addresses[0] != "192.0.2.1" {
			t.Fatalf("preview replaced verified target: %+v", peer)
		}
		other, ok := d.lookup("preview:other")
		if !ok || other.verified || other.nodeID != "preview:other" {
			t.Fatalf("preview lost its own record key: %+v", other)
		}
	}
}

func TestPeerDirectorySetAndLookup(t *testing.T) {
	d := newPeerDirectory()
	d.set([]noderec.DirectoryNode{
		{
			HostUUID: "uuid-b", Name: "nodeB", IP: "192.168.1.42", IdentityVerification: "verified", Trusted: true, PeerRecordID: "b-record",
			IPs: []string{"192.168.1.42", "10.0.0.42"}, ClusterUUID: "cuuid-b",
			Services: map[noderec.ServiceKey]noderec.ServiceStatus{
				noderec.ServiceEngineControl: {Port: 14323},
			},
		},
		{ // advertises em but not ec -> excluded
			HostUUID: "uuid-c", Name: "nodeC", IP: "192.168.1.43", IdentityVerification: "verified", Trusted: true, PeerRecordID: "c-record",
			Services: map[noderec.ServiceKey]noderec.ServiceStatus{
				noderec.ServiceEngineManager: {Port: 14322},
			},
		},
		{ // ec but no dialable IP -> excluded
			HostUUID: "uuid-d", Name: "nodeD", IdentityVerification: "verified", Trusted: true, PeerRecordID: "d-record",
			Services: map[noderec.ServiceKey]noderec.ServiceStatus{
				noderec.ServiceEngineControl: {Port: 14323},
			},
		},
	})

	p, ok := d.lookup("uuid-b")
	if !ok || !slices.Equal(p.addresses, []string{"192.168.1.42", "10.0.0.42"}) ||
		p.port != 14323 || p.clusterUUID != "cuuid-b" {
		t.Fatalf("unexpected nodeB entry: %+v ok=%v", p, ok)
	}
	if _, ok := d.lookup("uuid-c"); ok {
		t.Fatal("nodeC advertises no ec; should not be in directory")
	}
	if _, ok := d.lookup("uuid-d"); ok {
		t.Fatal("nodeD has no dialable IP; should not be in directory")
	}

	// A later snapshot replaces the set wholesale.
	d.set(nil)
	if _, ok := d.lookup("uuid-b"); ok {
		t.Fatal("empty snapshot should clear the directory")
	}
}

// TestPeerDirectoryKeysByHostUUID: an ec peer with a stable hostUuid is keyed
// (and addressed) by it, not the hostname — so a remote-control target survives
// a rename and same-named peers stay distinct. Invalid record keys are excluded.
func TestPeerDirectoryKeysByHostUUID(t *testing.T) {
	d := newPeerDirectory()
	d.set([]noderec.DirectoryNode{{
		HostUUID: "uuid-b", Name: "nodeB", IP: "192.168.1.42", ClusterUUID: "cuuid-b", IdentityVerification: "verified", Trusted: true, PeerRecordID: "b-record",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceEngineControl: {Port: 14323},
		},
	}})
	p, ok := d.lookup("uuid-b")
	if !ok || p.nodeID != "uuid-b" || !slices.Equal(p.addresses, []string{"192.168.1.42"}) {
		t.Fatalf("expected lookup by hostUuid, got %+v ok=%v", p, ok)
	}
	if _, ok := d.lookup("nodeB"); ok {
		t.Fatal("must not be addressable by hostname when a hostUuid is present")
	}
}
