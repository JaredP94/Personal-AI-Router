// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"os/exec"
	"time"
)

// Even a caller without a context deadline cannot wait forever for a helper.
// More restrictive caller deadlines still cancel the entire owned job.
const windowsCommandLimit = 15 * time.Minute

func runOwnedCommand(cmd *exec.Cmd) error { return runOwnedCommandMode(cmd, false) }

func runOwnedCommandMode(cmd *exec.Cmd, detachedControl bool) error {
	owner, err := configureProcessJob(cmd, detachedControl)
	if err != nil {
		return err
	}
	return runJobCommand(cmd, owner, windowsCommandLimit)
}
