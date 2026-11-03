// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"syscall"
	"testing"
)

// Reusing a device/inode must not transfer an install claim to a later bundle.
func TestFileIdentityDistinguishesReusedInode(t *testing.T) {
	st := syscall.Stat_t{Dev: 1, Ino: 2, Birthtimespec: syscall.Timespec{Sec: 3, Nsec: 4}}
	original := fileIdentityFromStat(&st)
	if got := fileIdentityFromStat(&st); got != original {
		t.Fatal("unchanged bundle identity is unstable")
	}
	st.Ctimespec = syscall.Timespec{Sec: 5, Nsec: 6}
	if got := fileIdentityFromStat(&st); got != original {
		t.Fatal("metadata changes must not invalidate a Darwin bundle's identity")
	}
	st.Birthtimespec.Nsec++
	if got := fileIdentityFromStat(&st); got == original {
		t.Fatal("reused inode with a different birth nanosecond retained the old identity")
	}
	st.Birthtimespec.Nsec--
	st.Birthtimespec.Sec++
	if got := fileIdentityFromStat(&st); got == original {
		t.Fatal("reused inode with a different birth second retained the old identity")
	}
}
