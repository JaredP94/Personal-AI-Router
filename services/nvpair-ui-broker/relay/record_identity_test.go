// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package relay

import (
	"testing"

	"nvpair-shared/noderec"
)

// A claimed host UUID must not let an independent preview overwrite or remove
// the verified peer. These are projection fixtures, not TLS proof fixtures.
func TestDirectoryPreviewRemovalPreservesVerifiedRecord(t *testing.T) {
	d := NewDirectory()
	verified := noderec.DirectoryNode{
		HostUUID: "peer", ClusterUUID: "peer", Name: "owned-peer",
		IdentityVerification: "verified", PeerRecordID: "current-pin-record", Trusted: true,
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceOllama: {Port: 11434}},
		Models:   []string{"owned-model"},
	}
	preview := noderec.DirectoryNode{
		HostUUID: "peer", Name: "unproven-claim", IdentityVerification: "unverified",
		PeerRecordID: noderec.PreviewRecordID("unproven-claim", "peer"),
		Services:     map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceOllama: {Port: 9999}},
		Models:       []string{"unproven-model"},
	}
	d.Apply(noderec.NotifyNodeDiscovered, verified)
	d.Apply(noderec.NotifyNodeDiscovered, preview)
	if all := d.Snapshot(""); len(all) != 2 {
		t.Fatalf("verified record and independent preview must coexist, got %+v", all)
	}
	assertVerified := func() {
		t.Helper()
		nodes := d.Snapshot(noderec.ServiceOllama)
		if len(nodes) != 1 || nodes[0].Name != "owned-peer" || nodes[0].PeerRecordID != "current-pin-record" || nodes[0].Services[noderec.ServiceOllama].Port != 11434 || len(nodes[0].Models) != 1 || nodes[0].Models[0] != "owned-model" {
			t.Fatalf("verified service inventory was replaced by a claimed UUID: %+v", nodes)
		}
	}
	assertVerified()
	d.Apply(noderec.NotifyNodeRemoved, preview)
	if all := d.Snapshot(""); len(all) != 1 || all[0].IdentityVerification != "verified" {
		t.Fatalf("preview removal must retain the verified record: %+v", all)
	}
	assertVerified()
}

func TestDirectoryWithdrawalUsesOldProofToken(t *testing.T) {
	d := NewDirectory()
	node := noderec.DirectoryNode{
		HostUUID: "peer", ClusterUUID: "peer", IdentityVerification: "verified",
		PeerRecordID: "old-pin-record", Trusted: true,
	}
	d.Apply(noderec.NotifyNodeDiscovered, node)
	wrongToken := node
	wrongToken.PeerRecordID = "replacement-pin-record"
	d.Apply(noderec.NotifyNodeRemoved, wrongToken)
	if nodes := d.Snapshot(""); len(nodes) != 1 || nodes[0].PeerRecordID != "old-pin-record" {
		t.Fatalf("a different proof token removed the old record: %+v", nodes)
	}
	d.Apply(noderec.NotifyNodeRemoved, node)
	if nodes := d.Snapshot(""); len(nodes) != 0 {
		t.Fatalf("withdrawal using the old record's token left inventory: %+v", nodes)
	}
}
