// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"testing"
)

func TestNodeProvenanceKeepsPreviewSeparateFromVerifiedPeer(t *testing.T) {
	var nodes []availableNode
	err := json.Unmarshal([]byte(`[{"id":"claim","hostUuid":"peer","peerRecordId":"preview:claim","identityVerification":"unverified","trusted":false},{"id":"peer","hostUuid":"peer","peerRecordId":"peer:proof","identityVerification":"verified","trusted":true}]`), &nodes)
	if err != nil {
		t.Fatal(err)
	}
	view := newNodesView(nil)
	view.SetSize(100, 30)
	view.setNodes(nodes)
	if len(view.nodes) != 2 || view.nodes[0].PeerRecordID == view.nodes[1].PeerRecordID {
		t.Fatal("preview collided with verified row")
	}
	if nodeStatus(nodes[0]) != "Unverified" || nodeStatus(nodes[1]) != "Connected" {
		t.Fatal("provenance labels do not distinguish trust")
	}
	if nodeStatus(availableNode{IdentityVerification: "manual"}) != "Manual engine" {
		t.Fatal("manual bypass is not separately labelled")
	}
}
