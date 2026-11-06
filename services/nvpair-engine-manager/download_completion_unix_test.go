// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestLMSCompletionStopsDownloaderChildAfterLauncherExit(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "launcher")
	childMarker := marker + "-child"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ready := make(chan struct{}, 1)
	done := make(chan error, 1)
	output := &lmsDownloadOutput{lastPercent: -1, progress: func(int) {
		select {
		case ready <- struct{}{}:
		default:
		}
	}}
	bin := fakeEngineForTest(t)
	go func() {
		_, err := runLMSDownloadCommand(ctx, []string{bin, "shutdown-download-fixture", marker, "exit-parent", childMarker}, output)
		done <- err
	}()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("launcher did not start")
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	childData, err := os.ReadFile(childMarker + "-pid")
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(string(childData))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { killOwnedTestPID(childPID, bin) })
	address, err := os.ReadFile(childMarker)
	if err != nil {
		t.Fatal(err)
	}
	launcher, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	// Cause launcher exit without cancelling the PAIR operation: cleanup must
	// also run through the normal completion path, not just cancel/shutdown.
	if err := launcher.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("normal completion did not finish")
	}
	conn, err := net.DialTimeout("tcp", string(address), 100*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatal("finished download left its helper running")
	}
}
