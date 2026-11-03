// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "syscall"

// Stat_t has no birth time on Linux. ctime distinguishes reused inodes and
// stays stable while the installed bundle is unchanged.
func fileGenerationTime(st *syscall.Stat_t) syscall.Timespec {
	return st.Ctim
}
