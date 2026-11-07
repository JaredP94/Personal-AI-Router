// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestLlamaControlledChildEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("macOS-only engine; Unix env fixture")
	}
	for _, name := range []string{"LLAMA_ARG_REUSE_PORT", "LLAMA_ARG_TOOLS_RUNTIME", "LLAMA_ARG_CONNECT", "LLAMA_ARG_HF_REPO", "LLAMA_CACHE", "HF_TOKEN", "HF_ENDPOINT", "MODEL_ENDPOINT", "MTMD_BACKEND_DEVICE", "LLAMA_APP_CMD", "GGML_BACKEND_DL_PATH", "DYLD_INSERT_LIBRARIES", "CUSTOM"} {
		t.Setenv(name, "unwanted")
	}
	var mu sync.Mutex
	var lines []string
	proc, err := startManagedProcWithEnvironment("/usr/bin/env", nil, map[string]string{"LLAMA_CACHE": "/fixture-models", "LLAMA_ARG_REUSE_PORT": "0"}, false, func(_, line string) { mu.Lock(); defer mu.Unlock(); lines = append(lines, line) })
	if err != nil {
		t.Fatal(err)
	}
	<-proc.done
	mu.Lock()
	defer mu.Unlock()
	for _, line := range lines {
		if strings.HasSuffix(line, "=unwanted") {
			t.Errorf("inherited setting escaped: %s", strings.SplitN(line, "=", 2)[0])
		}
	}
	if len(lines) != 2 {
		t.Fatalf("expected exactly controlled environment, got %d variables", len(lines))
	}
}

func TestLlamaConfigRefused(t *testing.T) {
	for _, body := range []string{"reuse-port = true\ntools-runtime = /fixture/runtime\n", "[server]\nreuse-port = true\n"} {
		path := filepath.Join(t.TempDir(), "config.ini")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := rejectLlamaConfigFiles(path); err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("ambient config accepted: %v", err)
		}
	}
	path := filepath.Join(t.TempDir(), "config.ini")
	if err := rejectLlamaConfigFiles(path); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(path+".missing", path); err != nil {
			t.Fatal(err)
		}
		if err := rejectLlamaConfigFiles(path); err == nil {
			t.Fatal("dangling config accepted")
		}
	}
}

func TestLlamaAmbientConfigNeutralized(t *testing.T) {
	home := t.TempDir()
	xdg := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	for _, base := range []string{filepath.Join(home, ".config"), xdg} {
		dir := filepath.Join(base, "llama.cpp")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.ini"), []byte("reuse-port = true\ntools-runtime = /fixture/runtime\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	p, _ := llamaManifest(t).PlatformFor("darwin", "arm64")
	root := t.TempDir()
	cmd, err := resolveProcessLaunch(p.Runtime, "/fixture/server", map[string]string{"host": "127.0.0.1", "port": "8082", "install_dir": root})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Env["HOME"] != filepath.Join(root, ".pair-home") || cmd.Env["XDG_CONFIG_HOME"] != filepath.Join(root, ".pair-config") {
		t.Fatalf("ambient config search still reachable: %+v", cmd.Env)
	}
	if cmd.Env["LLAMA_ARG_REUSE_PORT"] != "0" || cmd.Env["LLAMA_ARG_API_PREFIX"] != "" {
		t.Fatal("managed settings missing")
	}
}
