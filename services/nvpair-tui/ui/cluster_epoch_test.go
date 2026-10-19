// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"strings"
	"testing"
)

func TestClusterLeaveEpochRecoveryInvalidatesConfirmation(t *testing.T) {
	for _, failIdentity := range []bool{false, true} {
		t.Run(map[bool]string{false: "delayed-identity", true: "failed-identity"}[failIdentity], func(t *testing.T) {
			b, l := newRecoverySession(t)
			b.mu.Lock()
			b.clusterID = "cluster-A"
			b.mu.Unlock()
			b.push(t, "A", 1, "heartbeat", nil)
			l.until(t, func() bool { return l.m.hydratedEpoch == "A" })
			l.m.active = 6
			cl := l.m.views[6].(*clusterView)
			l.turn(t, engineKey("L"))
			if !strings.Contains(cl.View(), "Leave cluster?") {
				t.Fatal("initial leave confirmation missing")
			}

			entry := b.hold("cluster:get-node-id")
			b.mu.Lock()
			b.clusterID = "cluster-B"
			if failIdentity {
				b.fail = "cluster:get-node-id"
			}
			b.mu.Unlock()
			b.push(t, "B", 2, "heartbeat", nil)
			l.until(t, func() bool { return l.m.epoch == "B" })
			awaitEntry(t, entry)
			if cl.confirm.open() || cl.leaveIdentity != nil {
				t.Fatal("epoch change retained the old leave confirmation before identity rehydrated")
			}
			l.turn(t, engineKey("y"))
			l.turn(t, engineKey("L"))
			if cl.confirm.open() {
				t.Fatal("leave can be confirmed against unhydrated identity")
			}
			// This partial push does not establish this node's principal.
			b.push(t, "B", 3, "cluster:identity-changed", map[string]string{"clusterId": "cluster-B"})
			l.until(t, func() bool { return cl.identity.ClusterID == "cluster-B" })
			l.turn(t, engineKey("L"))
			if cl.confirm.open() {
				t.Fatal("partial identity push unblocked leave before authoritative snapshot")
			}
			b.unhold()
			l.until(t, func() bool { return l.m.hydratedEpoch == "B" })
			l.turn(t, engineKey("y"))
			if b.count("cluster:leave") != 0 {
				t.Fatal("old confirmation dispatched leave after recovery")
			}
			l.turn(t, engineKey("L"))
			if failIdentity {
				if cl.confirm.open() {
					t.Fatal("failed identity getter unblocked leave")
				}
				return
			}
			if !strings.Contains(cl.confirm.View(), "cluster-B") || !strings.Contains(cl.confirm.View(), "B") {
				t.Fatalf("new confirmation did not show recovered identity: %s", cl.View())
			}
			l.turn(t, engineKey("y"))
			l.until(t, func() bool { return cl.status == "leave cluster ok" })
			if b.count("cluster:leave") != 1 {
				t.Fatalf("confirmed new leave count = %d", b.count("cluster:leave"))
			}
		})
	}
}
