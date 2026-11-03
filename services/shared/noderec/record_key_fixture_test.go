// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package noderec

import (
	"encoding/json"
	"os"
	"testing"
)

// The desktop consumes this same table: neither implementation computes expectations.
func TestRecordKeyDesktopParity(t *testing.T) {
	data, err := os.ReadFile("../../../desktop/src/shared/fixtures/node-record-keys.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name                 string `json:"name"`
			HostUUID             string `json:"hostUuid"`
			PeerRecordID         string `json:"peerRecordId"`
			IdentityVerification string `json:"identityVerification"`
			Want                 string `json:"want"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) == 0 {
		t.Fatal("empty record-key fixture")
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			node := DirectoryNode{HostUUID: c.HostUUID, PeerRecordID: c.PeerRecordID, IdentityVerification: c.IdentityVerification}
			if got := node.RecordKey(); got != c.Want {
				t.Fatalf("RecordKey() = %q, want %q", got, c.Want)
			}
			if got := RecordSelector(c.Want).RecordKey(); got != c.Want {
				t.Fatalf("RecordSelector round trip = %q, want %q", got, c.Want)
			}
		})
	}
}
