// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type downloadProcess struct {
	cmd         *exec.Cmd
	signalMu    sync.Mutex
	finalSignal bool
}

func configureDownloadProcess(cmd *exec.Cmd) (*downloadProcess, error) {
	configureSysProcAttr(cmd)
	return &downloadProcess{cmd: cmd}, nil
}
func (*downloadProcess) close() {}

func killDownload(_ context.Context, process *downloadProcess) error {
	return signalDownload(process, syscall.SIGKILL)
}
func interruptDownload(_ context.Context, process *downloadProcess) error {
	return signalDownload(process, syscall.SIGINT)
}
func terminateDownload(_ context.Context, process *downloadProcess) error {
	return signalDownload(process, syscall.SIGTERM)
}
func handleDownloadProcess() bool { return false }

func pinDownloadProcess(*downloadProcess) (func(), error) { return func() {}, nil }

// Setpgid makes the launcher PID the group ID. The launcher is kept alive or
// unreaped until SIGKILL, anchoring that identity even when no children remain.
// Shutdown can interrupt cancellation after SIGKILL/reap has started: never
// signal the group again then, because the pgid may already have been reused.
// This lock covers only signals, never exit notification, reap or verification.
func signalDownload(process *downloadProcess, sig syscall.Signal) error {
	process.signalMu.Lock()
	defer process.signalMu.Unlock()
	if process.finalSignal || process.cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-process.cmd.Process.Pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		err = nil // The whole group is gone.
	}
	if sig == syscall.SIGKILL {
		process.finalSignal = true
	}
	return err
}

func waitDownloadGroup(ctx context.Context, process *downloadProcess) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	pgid := process.cmd.Process.Pid
	for {
		err := syscall.Kill(-pgid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil && !errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("probe download process group %d: %w", pgid, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("download process group %d still exists: %w", pgid, ctx.Err())
		case <-ticker.C:
		}
	}
}
