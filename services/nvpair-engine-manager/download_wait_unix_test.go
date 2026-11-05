// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package main

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// A concurrent cmd.Wait used for exit notification releases the PID before the
// final group signal. With no children, there is nothing else to anchor the pgid.
func TestDownloadExitNotificationKeepsLauncherUnreaped(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	process, err := configureDownloadProcess(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer process.close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w := newDownloadWait(cmd)
	t.Cleanup(func() {
		_ = killDownload(context.Background(), process)
		w.reap()
		select {
		case <-w.finished:
		case <-time.After(3 * time.Second):
			t.Error("launcher not reaped")
		}
	})
	if err := watchDownloadExit(w); err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.exited:
	case <-time.After(3 * time.Second):
		t.Fatal("no launcher exit notification")
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("exit notification reaped the launcher before the final group signal: %v", err)
	}
	if err := killAndWaitDownload(context.Background(), process, w, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err == nil {
		t.Fatal("launcher not reaped after the final group signal")
	}
}
