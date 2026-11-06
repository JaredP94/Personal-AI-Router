// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"io"
	"os/exec"
	"sync"
	"time"
)

// Preserve Unix orphan reclaim and its existing signal/deadline semantics.
func terminateOwnedPID(pid int, grace time.Duration, _ string) {
	if pid <= 0 {
		return
	}
	_ = signalPID(pid, false)
	if grace <= 0 {
		grace = 5 * time.Second
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	if pidAlive(pid) {
		_ = signalPID(pid, true)
	}
}

func startEngineCommand(cmd *exec.Cmd) (*processJob, error) { return nil, cmd.Start() }
func waitEngineCommand(mp *managedProc, scans *sync.WaitGroup, _, _ io.Closer) {
	scans.Wait()
	_ = mp.cmd.Wait()
}
func stopEngineCommand(mp *managedProc) { _ = gracefulSignal(mp.cmd); <-mp.done }
