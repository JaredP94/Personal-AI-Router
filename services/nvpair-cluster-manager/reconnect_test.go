// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"
)

// The real mTLS endpoint learns only the authenticated source host and keeps
// the paired listening port, never the ephemeral client port.

func TestHandleRosterLearnsSourceAddr(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mA := newTestManagerPort(t, 15021)
	mB := newTestManagerPort(t, 15022)
	go func() { _ = mA.runHTTP(ctx) }()
	go func() { _ = mB.runHTTP(ctx) }()
	time.Sleep(400 * time.Millisecond)

	pinTrusted(t, mA, mB.identity.NodeUUID, string(mB.identity.CertPEM), mB.identity.CertFingerprint)
	pinTrusted(t, mB, mA.identity.NodeUUID, string(mA.identity.CertPEM), mA.identity.CertFingerprint)

	// mA's record for mB points at a stale IP; mB still reaches mA fine.
	mA.upsertMember(&ClusterNode{NodeUUID: mB.identity.NodeUUID, ID: "node-b", IPAddress: "10.9.9.9", Port: 15022, ClusterID: "cluster-1", AdmissionEpoch: 1, State: stateMember})
	mB.upsertMember(&ClusterNode{NodeUUID: mA.identity.NodeUUID, ID: "node-a", IPAddress: "127.0.0.1", Port: 15021, ClusterID: "cluster-1", AdmissionEpoch: 1, State: stateMember})

	mB.reconcileWith([]string{net.JoinHostPort("127.0.0.1", strconv.Itoa(15021))}, mA.identity.NodeUUID)

	n, ok := mA.memberByNodeID(mB.identity.NodeUUID)
	if !ok {
		t.Fatal("mA lost mB membership")
	}
	if n.IPAddress != "127.0.0.1" {
		t.Fatalf("mA did not learn mB's source IP: got %q want 127.0.0.1", n.IPAddress)
	}
	if n.Port != 15022 {
		t.Fatalf("mA must keep mB's listening port, not the ephemeral source port: got %d want 15022", n.Port)
	}
}

// TestRefreshMemberAddrsFromMDNS verifies the discovery backstop: a member
// whose stored address is stale is refreshed from the mDNS browse map, the
// pass is a safe no-op without a browser, and the self entry is never rewritten.
