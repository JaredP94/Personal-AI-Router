// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os/exec"
	"time"
)

// runJobCommand is the platform-independent ownership lifetime. Activation is
// before Wait (and before the gated child can execute); every exit path releases
// the job. The owner serializes handle operations only, never Start or Wait.
func runJobCommand(cmd *exec.Cmd, owner *processJob, limit time.Duration) error {
	defer owner.release()
	if cmd.Cancel != nil {
		cmd.Cancel = owner.stop
	}
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := owner.activate(); err != nil {
		_ = cmd.Process.Kill() // Original process handle, never a PID lookup.
		_ = cmd.Wait()
		return err
	}
	timer := time.AfterFunc(limit, func() { _ = owner.stop() })
	defer timer.Stop()
	return cmd.Wait()
}
