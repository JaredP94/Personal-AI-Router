// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"
)

func TestPairingCompletionPublishesOriginalHostnameForCleanup(t *testing.T) {
	for _, commit := range []bool{true, false} {
		t.Run(map[bool]string{true: "committed", false: "abandoned"}[commit], func(t *testing.T) {
			m := newTestManager(t)
			peer := newPinFixture(t, "peer")
			pi, cert := pairingInfoFor(t, peer)
			pi.Addr = "192.0.2.50:14321" // observed source, not the entered hostname
			const inviteID = "manual-hostname"
			m.putInvite(&Invite{InviteID: inviteID, ClusterID: "cluster-1", State: inviteStatePending})
			sess := putInviterSession(t, m, inviteID, "cluster-1")
			sess.joinerAddr = "Workstation.tail.example:14321"
			if !commit {
				m.setClusterIdentity("", "")
			}
			var out bytes.Buffer
			m.codec = NewCodec(&out)
			err := m.finalizePairing(inviteID, sess, pi, cert)
			if commit && err != nil {
				t.Fatal(err)
			}
			if !commit && err == nil {
				t.Fatal("abandoned pairing committed")
			}
			found := false
			decoder := json.NewDecoder(&out)
			for {
				var event struct {
					Method string `json:"method"`
					Params struct {
						Address  string `json:"address"`
						PeerUUID string `json:"peerUuid"`
					} `json:"params"`
				}
				err := decoder.Decode(&event)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if event.Method != "cluster:pairing-completed" {
					continue
				}
				found = true
				if event.Params.Address != "Workstation.tail.example" || event.Params.PeerUUID != peer.uuid {
					t.Fatalf("cleanup association lost original address or authenticated peer: %+v", event.Params)
				}
			}
			if found != commit {
				t.Fatalf("cleanup association emitted=%v after committed=%v", found, commit)
			}
		})
	}
}
