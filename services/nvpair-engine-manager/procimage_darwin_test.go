// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseLsofImage(t *testing.T) {
	out := "p123\nftxt\nn/Users/me/Library/engine-bin/ollama/Ollama.app/Contents/Resources/ollama\nftxt\nn/usr/lib/dyld\n"
	if got := parseLsofImage(out); got != "/Users/me/Library/engine-bin/ollama/Ollama.app/Contents/Resources/ollama" {
		t.Fatalf("image = %q", got)
	}
	if got := parseLsofImage("p123\nfcwd\nn/tmp\n"); got != "" {
		t.Fatalf("a non-txt descriptor was taken as the image: %q", got)
	}
	if got := parseLsofImage("ftxt\nn/dir/engine \n"); got != "/dir/engine " {
		t.Fatalf("a trailing space must survive, got %q", got)
	}
	if got := parseLsofImage(""); got != "" {
		t.Fatalf("empty output gave %q", got)
	}
}

// The running test binary is a process whose image is known.
func TestProcImageResolvesARunningProcess(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(exe)
	if err != nil {
		t.Fatal(err)
	}
	if got := normalizeEngineImage(procImage(os.Getpid())); got != want {
		t.Fatalf("procImage(self) = %q, want %q", got, want)
	}
	if got := procImage(0); got != "" {
		t.Fatalf("procImage(0) = %q, want empty", got)
	}
}
