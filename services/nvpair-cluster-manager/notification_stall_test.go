// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net"
	"testing"
	"time"
)

func TestPairingCompletionStalledReaderReleasesRosterMu(t *testing.T) {
	m := newTestManager(t)
	peer := newPinFixture(t, "peer")
	pi, cert := pairingInfoFor(t, peer)
	m.putInvite(&Invite{InviteID: "stalled", ClusterID: "cluster-1", State: inviteStatePending})
	sess := putInviterSession(t, m, "stalled", "cluster-1")
	local, remote := net.Pipe()
	m.codec = NewCodec(local)
	t.Cleanup(func() { _ = remote.Close(); _ = m.codec.Close() })
	done := make(chan error, 1)
	go func() { done <- m.finalizePairing("stalled", sess, pi, cert) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("pairing completion blocked under rosterMu on stalled stdout")
	}
	if !m.rosterMu.TryLock() {
		t.Fatal("pairing completion retained rosterMu")
	}
	m.rosterMu.Unlock()
}
