// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package main

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestDownloadCleanupDoesNotSignalAfterReap(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	process, err := configureDownloadProcess(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	wait := newDownloadWait(cmd)
	if err := killAndWaitDownload(context.Background(), process, wait, time.Second); err != nil {
		t.Fatal(err)
	}

	// Model reuse of the released numeric pgid with another owned group. This
	// does not depend on the kernel cycling through its entire PID namespace.
	other := exec.Command("/bin/sh", "-c", "exec sleep 30")
	otherProcess, err := configureDownloadProcess(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = other.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = killDownload(context.Background(), otherProcess)
		<-done
	})
	process.cmd = other
	for _, signal := range []func(context.Context, *downloadProcess) error{
		interruptDownload, terminateDownload, killDownload,
	} {
		if err := signal(context.Background(), process); err != nil {
			t.Fatal(err)
		}
	}
	// SIGINT/TERM/KILL must not have reached the replacement group. A Wait
	// with a short deadline distinguishes a survivor from a pending exit.
	select {
	case <-done:
		t.Fatal("cleanup signalled a replacement process group after reap")
	case <-time.After(50 * time.Millisecond):
	}
}
