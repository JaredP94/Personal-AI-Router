// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestCommandStopAndRestartWithPersistentHost(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "server")
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(fakeEngineForTest(t), "persistent-host", marker, "127.0.0.1:"+strconv.Itoa(port))
	configureSysProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { killTestProcessGroup(cmd.Process.Pid); <-done })
	waitPortServing(t, port)
	if pid, _, ok := pidOnPort(port); !ok || pid != cmd.Process.Pid {
		t.Skip("host cannot resolve listener PID")
	}
	m := testEngineManifest(fakeEngineForTest(t))
	p := m.Platforms[hostKey()]
	p.Runtime.Mode = "command"
	p.Runtime.Port = port
	p.Runtime.Start = [][]string{{fakeEngineForTest(t), "touch", marker + "-start"}}
	p.Runtime.Stop = &StopSpec{Cmd: []string{fakeEngineForTest(t), "touch", marker + "-stop"}, GraceS: 1}
	p.Runtime.Health = nil
	m.Platforms[hostKey()] = p
	ex := newTestExecutor(t, m)
	st, _ := ex.state("fake")
	st.running, st.port = true, port

	if err := ex.Stop("fake"); err != nil {
		t.Fatalf("stop must succeed when the server closes but its host stays alive: %v", err)
	}
	if status, _ := ex.Status("fake"); status.Running || status.Healthy || portServing(port) {
		t.Fatalf("stopped server must be reported down: %+v", status)
	}
	select {
	case <-done:
		t.Fatal("stop terminated the persistent host")
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ex.Start(ctx, "fake"); err != nil {
		t.Fatalf("start after stop: %v", err)
	}
	if err := ex.Restart(ctx, "fake"); err != nil {
		t.Fatalf("restart with a persistent host: %v", err)
	}
	if status, _ := ex.Status("fake"); !status.Running || !status.Healthy || !portServing(port) {
		t.Fatalf("restarted server must be healthy: %+v", status)
	}
	if pid, _, ok := pidOnPort(port); !ok || pid != cmd.Process.Pid {
		t.Fatalf("restart must reuse the persistent host, got pid=%d resolved=%v", pid, ok)
	}
}

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
	cmd := exec.Command(fakeEngineForTest(t), "shutdown-tail", marker, "127.0.0.1:"+strconv.Itoa(port), delay)
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
	m := testEngineManifest(fakeEngineForTest(t))
	p := m.Platforms[hostKey()]
	p.Runtime.Mode = "command"
	p.Runtime.Port = port
	p.Runtime.Stop = &StopSpec{Cmd: []string{fakeEngineForTest(t), "touch", marker}, GraceS: grace, WaitForExit: true}
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
