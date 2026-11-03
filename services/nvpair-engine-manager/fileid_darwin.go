// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "syscall"

// Birth time survives the staging-to-destination rename and metadata changes.
func fileGenerationTime(st *syscall.Stat_t) syscall.Timespec {
	return st.Birthtimespec
}
