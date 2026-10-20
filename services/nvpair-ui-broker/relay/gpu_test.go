// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package relay

import (
	"testing"

	"nvpair-shared/noderec"
)

func TestDirectoryGPUInventoryUsesSharedEquality(t *testing.T) {
	equal := func(a, b []noderec.GPUInfo) bool {
		return directoryNodeEqual(noderec.DirectoryNode{GPUs: a}, noderec.DirectoryNode{GPUs: b})
	}
	// Separate pointers with the same reading must not trigger an update.
	idleA, idleB := uint32(0), uint32(0)
	a := []noderec.GPUInfo{{MemoryKind: "dedicated", UtilizationPercent: &idleA}}
	b := []noderec.GPUInfo{{MemoryKind: "dedicated", UtilizationPercent: &idleB}}
	if !equal(a, b) {
		t.Fatal("equal readings at different addresses must compare equal")
	}
	b[0].UtilizationPercent = nil
	if equal(a, b) {
		t.Fatal("an unknown reading must differ from measured idle")
	}
	if !equal(nil, nil) || equal(nil, a) || equal(a, nil) {
		t.Fatal("inventory equality must account for appearance and disappearance")
	}
}
