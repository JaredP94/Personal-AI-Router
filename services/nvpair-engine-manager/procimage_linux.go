// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"os"
	"strconv"
)

// procImage resolves a PID's executable path via /proc. Empty on any error, so
// the caller's image check fails closed.
func procImage(pid int) string {
	if pid <= 0 {
		return ""
	}
	if path, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe"); err == nil {
		return path
	}
	return ""
}
