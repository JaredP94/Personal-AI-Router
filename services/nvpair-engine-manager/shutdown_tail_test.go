// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestCommandStopWaitsForProcessExitAfterPortCloses(t *testing.T) {
	testCommandStopTail(t, "900ms", 2, false)
}

func TestCommandStopRetainsRunningOnTeardownTimeout(t *testing.T) {
	testCommandStopTail(t, "3s", 1, true)
}

func testCommandStopTail(t *testing.T, delay string, grace int, wantTimeout bool) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "stop")
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(fakeEngineBin, "shutdown-tail", marker, "127.0.0.1:"+strconv.Itoa(port), delay)
	configureSysProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = signalPID(cmd.Process.Pid, true); <-done })
	waitPortServing(t, port)
	if pid, _, ok := pidOnPort(port); !ok || pid != cmd.Process.Pid {
		t.Skip("host cannot resolve listener PID")
	}
	m := testEngineManifest(fakeEngineBin)
	p := m.Platforms[hostKey()]
	p.Runtime.Mode = "command"
	p.Runtime.Port = port
	p.Runtime.Stop = &StopSpec{Cmd: []string{fakeEngineBin, "touch", marker}, GraceS: grace}
	p.Runtime.Health = nil
	m.Platforms[hostKey()] = p
	ex := newTestExecutor(t, m)
	st, _ := ex.state("fake")
	st.running, st.port = true, port
	started := time.Now()
	err = ex.Stop("fake")
	if wantTimeout {
		if !errors.Is(err, errCommandStillRunning) {
			t.Fatalf("expected bounded shutdown timeout, got %v", err)
		}
		st.mu.Lock()
		running := st.running
		st.mu.Unlock()
		if !running {
			t.Fatal("timeout must not report stopped")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("stop exceeded lifecycle bound: %s", elapsed)
	}
	select {
	case <-done:
	default:
		t.Fatal("Stop reported stopped with the port closed but the engine process still shutting down; immediate restart would race")
	}
	if portServing(port) {
		t.Fatal("stop left the port occupied")
	}
}
