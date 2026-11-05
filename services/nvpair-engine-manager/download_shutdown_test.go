// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRunStopsDownloaderBeforeReturning(t *testing.T) {
	testRunStopsDownloader(t, false, false, false)
}
func TestRunBoundsDownloaderIgnoringSIGTERM(t *testing.T) {
	testRunStopsDownloader(t, true, false, false)
}
func TestRunAcceleratesAlreadyCancelingDownloader(t *testing.T) {
	testRunStopsDownloader(t, true, true, false)
}

func TestRunStopsDownloaderProcessGroup(t *testing.T) {
	testRunStopsDownloader(t, true, false, true)
}

func testRunStopsDownloader(t *testing.T, ignore, midCancel, tree bool) {
	t.Helper()
	managerConn, brokerConn := net.Pipe()
	defer managerConn.Close()
	defer brokerConn.Close()
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	m := NewManager(NewCodec(managerConn), ex, nil)
	runDone := make(chan error, 1)
	go func() { runDone <- m.Run(context.Background()) }()
	_ = brokerConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	codec := NewCodec(brokerConn)
	for i := 0; i < 2; i++ {
		if _, err := codec.Read(); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(t.TempDir(), "downloader-pid")
	childMarker := marker + "-child"
	events, unsub := ex.progress.subscribe("lmstudio")
	defer unsub()
	ready := make(chan struct{}, 1)
	output := &lmsDownloadOutput{lastPercent: -1, progress: func(int) {
		select {
		case ready <- struct{}{}:
		default:
		}
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pullDone := make(chan struct{})
	mode := "term"
	if ignore {
		mode = "ignore"
	}
	go func() {
		defer close(pullDone)
		_, _ = ex.trackedPull(ctx, "lmstudio", "fixture", "shutdown-pull", true, func(ctx context.Context) (json.RawMessage, error) {
			argv := []string{fakeEngineBin, "shutdown-download-fixture", marker, mode}
			if tree {
				argv = append(argv, childMarker)
			}
			_, err := runLMSDownloadCommand(ctx, argv, output)
			return nil, err
		})
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("downloader did not start")
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Kill()
		}
		cancel()
		select {
		case <-pullDone:
		case <-time.After(6 * time.Second):
			t.Error("downloader cleanup did not finish")
		}
	})
	if !pidAlive(pid) {
		t.Fatal("fixture was not running")
	}
	if midCancel && !ex.CancelModelPull("lmstudio", "fixture", "shutdown-pull") {
		t.Fatal("cancel not accepted")
	}
	childAddress := ""
	if tree {
		data, err := os.ReadFile(childMarker)
		if err != nil {
			t.Fatal(err)
		}
		childAddress = string(data)
		conn, err := net.DialTimeout("tcp", childAddress, time.Second)
		if err != nil {
			t.Fatal("child not alive before shutdown:", err)
		}
		_ = conn.Close()
	}
	started := time.Now()
	_ = brokerConn.Close() // the broker's normal worker-stop mechanism
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run exceeded shutdown bound")
	}
	if elapsed := time.Since(started); elapsed >= 4*time.Second {
		t.Fatalf("download shutdown exceeded 4s: %v", elapsed)
	}
	if pidAlive(pid) {
		t.Fatalf("downloader %d remained alive after Run returned", pid)
	}
	if tree {
		conn, err := net.DialTimeout("tcp", childAddress, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			t.Fatal("downloader child alive after Run returned")
		}
	}
	interrupted := false
	for len(events) > 0 {
		ev := <-events
		if ev.Stage == "cancelled" {
			t.Fatal("shutdown claimed cancellation succeeded")
		}
		interrupted = interrupted || ev.Stage == "interrupted"
	}
	if !interrupted {
		t.Fatal("no interrupted terminal update")
	}
	ex.pullMu.Lock()
	remaining := len(ex.pulls)
	ex.pullMu.Unlock()
	if remaining != 0 {
		t.Fatal("Run returned before the download stop sequence")
	}
}
