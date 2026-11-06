// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"slices"
	"testing"
)

func TestRoamingSuccessfulPairingFallbackBecomesMostRecent(t *testing.T) {
	m, b := newTestManager(t), newTestManager(t)
	roamingMember(t, m, b)
	roamingRequest(t, m, b, "10.0.0.2:50000", nil)
	roamingRequest(t, m, b, "192.0.2.20:50000", nil)
	n, _ := m.memberByNodeID(b.identity.NodeUUID)
	if got := m.resolvePeerAddrs(*n); !slices.Equal(got, []string{"192.0.2.20:14999", "10.0.0.2:14999"}) {
		t.Fatalf("most recent success not first: %v", got)
	}
}
