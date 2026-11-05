// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

type downloadProcess struct{ cmd *exec.Cmd }

func configureDownloadProcess(cmd *exec.Cmd) (*downloadProcess, error) {
	configureSysProcAttr(cmd)
	return &downloadProcess{cmd: cmd}, nil
}
func (*downloadProcess) close() {}

func killDownload(_ context.Context, process *downloadProcess) error {
	return signalDownload(process.cmd, syscall.SIGKILL)
}
func interruptDownload(_ context.Context, process *downloadProcess) error {
	return signalDownload(process.cmd, syscall.SIGINT)
}
func terminateDownload(_ context.Context, process *downloadProcess) error {
	return signalDownload(process.cmd, syscall.SIGTERM)
}
func handleDownloadProcess() bool { return false }

func pinDownloadProcess(*downloadProcess) (func(), error) { return func() {}, nil }

// Setpgid makes the launcher PID the group ID. A group ID cannot be reused
// while any member survives, so signal that group even after launcher reap;
// never look up a possibly reused launcher PID's current group.
func signalDownload(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil // The whole group is gone.
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
