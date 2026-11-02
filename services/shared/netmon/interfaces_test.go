// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package netmon

import (
	"net"
	"testing"
)

func TestSubscribeAfterShutdownIsAlreadyClosed(t *testing.T) {
	m := &Monitor{}
	m.closeSubs()
	select {
	case _, open := <-m.Subscribe():
		if open {
			t.Fatal("shutdown subscription still open")
		}
	default:
		t.Fatal("late subscription would hang shutdown")
	}
}

func TestInterfaceMetadataChangesInvalidateSnapshot(t *testing.T) {
	a := Snapshot{Interfaces: map[int]net.Interface{7: {Index: 7, Name: "eth0", Flags: net.FlagUp | net.FlagMulticast}}}
	b := a.clone()
	b.Interfaces[7] = net.Interface{Index: 7, Name: "eth0", Flags: net.FlagMulticast}
	if fingerprint(a) == fingerprint(b) {
		t.Fatal("down interface did not invalidate snapshot")
	}
	b = a.clone()
	b.Interfaces[7] = net.Interface{Index: 7, Name: "renamed", Flags: net.FlagUp | net.FlagMulticast}
	if fingerprint(a) == fingerprint(b) {
		t.Fatal("renamed interface did not invalidate snapshot")
	}
	if a.Interfaces[7].Name != "eth0" {
		t.Fatal("clone shared interface map")
	}
}
