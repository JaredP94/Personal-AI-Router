// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func testProcessImage(pid int) string { return procImage(pid) }

func testRunProcesses(root string) []fakeProcessRecord {
	out, err := exec.Command("ps", "-A", "-o", "pid=,args=").Output()
	if err != nil {
		return nil
	}
	var records []fakeProcessRecord
	for _, line := range strings.Split(string(out), "\n") {
		// Filter before the macOS lsof identity lookup: enumerating every system
		// image would cost one subprocess per unrelated desktop process.
		if !strings.Contains(line, root) && !strings.Contains(line, canonicalEnginePath(root)) {
			continue
		}
		values := strings.Fields(line)
		if len(values) == 0 {
			continue
		}
		pid, err := strconv.Atoi(values[0])
		if err != nil {
			continue
		}
		image := testProcessImage(pid)
		if image != "" && isManagedInstallPath(canonicalEnginePath(image), canonicalEnginePath(root)) {
			records = append(records, fakeProcessRecord{PID: pid, Image: image})
		}
	}
	return records
}

func killTestProcessGroup(pid int) {
	// A fixture without Setpgid may share the test runner's group. Never signal
	// that group; kill only the owned process in that case.
	if group, err := syscall.Getpgid(pid); err == nil && group != syscall.Getpgrp() {
		_ = syscall.Kill(-group, syscall.SIGKILL)
	} else {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}
