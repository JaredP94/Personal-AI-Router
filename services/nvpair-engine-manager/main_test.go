// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// Built once for the whole package: a pure-Go fake engine (stand-in for
// a real inference engine) and the engine-manager binary itself (for
// the cross-process e2e test).
var (
	fakeEngineBin   string
	managerBin      string
	testProcessRoot string
)

func TestMain(m *testing.M) {
	if handleDownloadProcess() {
		os.Exit(0)
	}
	tmp, err := os.MkdirTemp("", "nvpair-em-test-*")
	if err != nil {
		panic(err)
	}
	testProcessRoot = tmp
	if err := os.Setenv("PAIR_TEST_PROCESS_ROOT", tmp); err != nil {
		panic(err)
	}
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}

	fakeEngineBin = filepath.Join(tmp, "fake-engine"+suffix)
	if out, err := exec.Command("go", "build", "-o", fakeEngineBin, "./testdata/fakeengine").CombinedOutput(); err != nil {
		panic("build fake-engine: " + err.Error() + "\n" + string(out))
	}

	managerBin = filepath.Join(tmp, "nvpair-engine-manager"+suffix)
	if out, err := exec.Command("go", "build", "-o", managerBin, ".").CombinedOutput(); err != nil {
		panic("build engine-manager: " + err.Error() + "\n" + string(out))
	}

	code := m.Run()
	// Final safety net for detached children, including a test that missed its
	// per-test cleanup. Any survivor here makes the run fail, even if swept.
	// Include unregistered launchers that died before the fake could register.
	for _, record := range testRunProcesses(tmp) {
		fmt.Fprintf(os.Stderr, "test process survived: %+v\n", record)
		killTestProcessGroup(record.PID)
		code = 1
	}
	if alive := trackedFakeProcesses(tmp); len(alive) != 0 {
		fmt.Fprintf(os.Stderr, "fake engines survived their tests: %+v\n", alive)
		code = 1
	}
	if remaining := sweepFakeProcesses(tmp); len(remaining) != 0 {
		fmt.Fprintf(os.Stderr, "fake engine sweep failed: %+v\n", remaining)
		code = 1
	}
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}
