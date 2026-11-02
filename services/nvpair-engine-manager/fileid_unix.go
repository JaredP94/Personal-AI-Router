// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package main

import (
	"fmt"
	"os"
	"syscall"
)

// fileIdentity combines device, inode, and a generation timestamp without
// following a final link. The timestamp prevents inode reuse from transferring
// an install claim to a replacement bundle. It returns "" on read failure.
func fileIdentity(path string) string {
	info, err := os.Lstat(path)
	if err != nil {
		return ""
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fileIdentityFromStat(st)
}

func fileIdentityFromStat(st *syscall.Stat_t) string {
	stamp := fileGenerationTime(st)
	return fmt.Sprintf("%d:%d:%d:%d", st.Dev, st.Ino, stamp.Sec, stamp.Nsec)
}
