// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestOmlxStaleLauncherDoesNotRestore(t *testing.T) {
	base := resolutionRoot(t)
	home, _ := os.UserHomeDir()
	reg := isolatedCommandRegistry(t, "omlx", home)
	cli := filepath.Join(home, ".omlx/bin/omlx")
	resolutionFile(t, cli)
	target := filepath.Join(home, "Applications/oMLX.app/Contents/MacOS/omlx-cli")
	// Remove only our fixture, never the real app or the user's launcher.
	_ = os.Remove(target)
	assertCommandPresence(t, reg, base, "omlx", "", false)
	ex := reconciledTestExecutor(NewExecutor(reg, NewReporter(nil), nil, base))
	if err := ex.setDesiredEnabled("omlx", true); err != nil {
		t.Fatal(err)
	}
	if err := ex.RestoreEnabled(context.Background()); err != nil {
		t.Fatalf("missing app must not produce a launch error: %v", err)
	}
	if !fileExists(cli) {
		t.Fatal("stale launcher was removed")
	}
	resolutionFile(t, target)
	assertCommandPresence(t, reg, base, "omlx", cli, false)
	m, _ := reg.Get("omlx")
	p, _ := m.PlatformFor(runtime.GOOS, runtime.GOARCH)
	if got := detectedExecutable(p, filepath.Join(base, "omlx"), runtime.GOOS); got != cli {
		t.Fatalf("restored app: got %q", got)
	}
}
