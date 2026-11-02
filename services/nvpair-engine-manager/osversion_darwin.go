// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"strconv"
	"strings"
	"syscall"
)

// hostOSMajorVersion is the macOS major version (15, 26, ...), or 0 when it
// cannot be read, which matches only variants that declare no minimum.
func hostOSMajorVersion() int {
	v, err := syscall.Sysctl("kern.osproductversion")
	if err != nil {
		return 0
	}
	major, _, _ := strings.Cut(v, ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0
	}
	return n
}
