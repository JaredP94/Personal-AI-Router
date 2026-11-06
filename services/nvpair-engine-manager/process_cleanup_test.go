// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFakeEngineCleanupOnFailedTest(t *testing.T)         { testFakeEngineFailure(t, false) }
func TestFakeEngineTestMainSweepsMissedCleanup(t *testing.T) { testFakeEngineFailure(t, true) }

func testFakeEngineFailure(t *testing.T, sweepOnly bool) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFakeEngineFailureFixture$")
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = append(os.Environ(), "PAIR_TEST_FAILURE_MARKER="+marker, "PAIR_TEST_SWEEP_ONLY="+strconv.FormatBool(sweepOnly))
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "intentional failure after spawn") {
		t.Fatalf("fixture did not fail as intended: %v\n%s", err, out)
	}
	if sweepOnly && !strings.Contains(string(out), "test process survived") {
		t.Fatalf("TestMain failed to detect the missed cleanup: %s", out)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	var record fakeProcessRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	pid := record.PID
	t.Cleanup(func() {
		if isOurEngineImage(testProcessImage(pid), record.Image) {
			killTestProcessGroup(pid)
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for pidAlive(pid) && testProcessImage(pid) != "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if pidAlive(pid) && testProcessImage(pid) != "" {
		t.Fatalf("fake engine pid %d survived its failed test and test binary exit", pid)
	}
}

func TestFakeEngineFailureFixture(t *testing.T) {
	marker := os.Getenv("PAIR_TEST_FAILURE_MARKER")
	if marker == "" {
		return
	}
	var ex *Executor
	if os.Getenv("PAIR_TEST_SWEEP_ONLY") == "true" {
		// Intentionally bypass per-test cleanup to prove TestMain catches it.
		owner, err := os.MkdirTemp(testProcessRoot, "missed-")
		if err != nil {
			t.Fatal(err)
		}
		registerFakeExecutable(t, fakeEngineBin, owner)
		reg := NewRegistry()
		m := testEngineManifest(fakeEngineBin)
		reg.engines[m.Engine] = m
		ex = reconciledTestExecutor(NewExecutor(reg, NewReporter(nil), nil, t.TempDir()))
	} else {
		ex = newTestExecutor(t, testEngineManifest(fakeEngineForTest(t)))
	}
	if err := ex.Start(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	st, _ := ex.state("fake")
	st.mu.Lock()
	record := fakeProcessRecord{PID: st.proc.cmd.Process.Pid, Image: st.proc.cmd.Path}
	st.mu.Unlock()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Fatal("intentional failure after spawn")
}
