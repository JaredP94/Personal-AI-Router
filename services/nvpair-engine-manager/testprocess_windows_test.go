// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

func killTestProcessGroup(pid int) {
	// Identify and validate through the same handle, including for helpers
	// without listeners. Never re-open a PID after an image-only lookup.
	p, ok := openPinnedProcess(pid, 0)
	if !ok {
		return
	}
	defer p.close()
	image := canonicalEnginePath(p.imagePath())
	if !isManagedInstallPath(image, canonicalEnginePath(testProcessRoot)) {
		// Copied fixtures may live outside the run directory but must have
		// been explicitly registered to an owner inside this test run.
		key := fmt.Sprintf("%x", sha256.Sum256([]byte(image)))
		owner, err := os.ReadFile(filepath.Join(testProcessRoot, "owners", key))
		if err != nil || !isManagedInstallPath(canonicalEnginePath(string(owner)), canonicalEnginePath(testProcessRoot)) {
			return
		}
	}
	p.terminate(0, p.imagePath())
}
