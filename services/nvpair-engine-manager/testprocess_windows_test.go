// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os/exec"
	"strconv"
	"strings"
)

func testProcessImage(pid int) string { return imagePathForPID(pid) }

func testRunProcesses(root string) []fakeProcessRecord {
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "Get-Process | Select-Object -ExpandProperty Id").Output()
	if err != nil {
		return nil
	}
	var records []fakeProcessRecord
	for _, value := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(value)
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

func signalPID(pid int, _ bool) error { killTestProcessGroup(pid); return nil }

func killTestProcessGroup(pid int) { terminateOwnedPID(pid, 0, testProcessImage(pid)) }
