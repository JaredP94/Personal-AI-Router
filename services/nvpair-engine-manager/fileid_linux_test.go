// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"syscall"
	"testing"
)

// Linux can immediately reuse the inode of a deleted bundle directory.
func TestFileIdentityDistinguishesReusedInode(t *testing.T) {
	st := syscall.Stat_t{Dev: 1, Ino: 2, Ctim: syscall.Timespec{Sec: 3, Nsec: 4}}
	original := fileIdentityFromStat(&st)
	if got := fileIdentityFromStat(&st); got != original {
		t.Fatal("unchanged bundle identity is unstable")
	}
	st.Ctim.Nsec++
	if got := fileIdentityFromStat(&st); got == original {
		t.Fatal("reused inode with a different change nanosecond retained the old identity")
	}
	st.Ctim.Nsec--
	st.Ctim.Sec++
	if got := fileIdentityFromStat(&st); got == original {
		t.Fatal("reused inode with a different change second retained the old identity")
	}
}
