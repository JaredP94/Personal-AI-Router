// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package noderec

import "testing"

func TestRecordKeysSeparateClaimsPeersAndManualConfigurations(t *testing.T) {
	preview := DirectoryNode{HostUUID: "peer", Name: "claim", IdentityVerification: "unverified", PeerRecordID: PreviewRecordID("claim", "peer")}
	verified := DirectoryNode{HostUUID: "peer", IdentityVerification: "verified", Trusted: true, PeerRecordID: "peer:proof"}
	if preview.RecordKey() == verified.RecordKey() || preview.RecordKey() == preview.HostUUID || preview.ServicePeer() || !verified.ServicePeer() {
		t.Fatal("preview and peer identity namespaces overlap")
	}
	manual := DirectoryNode{HostUUID: "peer", IdentityVerification: "manual", PeerRecordID: ManualRecordID("peer")}
	if manual.RecordKey() == verified.RecordKey() || manual.RecordKey() == preview.RecordKey() || manual.ServicePeer() {
		t.Fatal("manual bypass overlaps peer identity")
	}
	for _, reserved := range []string{preview.RecordKey(), manual.RecordKey()} {
		collision := verified
		collision.HostUUID = reserved
		if collision.RecordKey() != "" || collision.ServicePeer() {
			t.Fatalf("peer principal claimed reserved record namespace %q", reserved)
		}
	}
	for _, node := range []DirectoryNode{preview, verified, manual} {
		if RecordSelector(node.RecordKey()).RecordKey() != node.RecordKey() {
			t.Fatal("record selector changed the target identity")
		}
	}
	if (DirectoryNode{HostUUID: "peer"}).RecordKey() != "" {
		t.Fatal("unmarked claim accepted a host key")
	}
}
