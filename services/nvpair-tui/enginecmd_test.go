// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

type engineObservation struct {
	Args      []string `json:"args"`
	Input     string   `json:"input"`
	Host      string   `json:"host"`
	Library   string   `json:"library"`
	Inherited string   `json:"inherited"`
	Path      string   `json:"path"`
	Dir       string   `json:"dir"`
	PID       int      `json:"pid"`
}

// Re-exec the test binary under sibling filenames, as supervisor_test.go does
// for its fake broker. The production nvpair-tui entrypoint is built separately.
func runEngineCommandTestHelper() {
	if os.Getenv("NVPAIR_ENGINE_TEST_HELPER") != "1" {
		return
	}
	switch strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") {
	case "nvpair-ui-broker":
		_ = os.WriteFile(os.Getenv("NVPAIR_ENGINE_BROKER_MARKER"), []byte("started"), 0o600)
		os.Exit(1)
	case "nvpair-engine-manager":
		mode := os.Getenv("NVPAIR_ENGINE_TEST_MODE")
		if mode == "resolver-error" {
			fmt.Fprintln(os.Stderr, "fixture is not installed")
			os.Exit(2)
		}
		if mode == "invalid-json" {
			fmt.Fprintln(os.Stdout, "not JSON")
			os.Exit(0)
		}
		if reflect.DeepEqual(os.Args[1:], []string{"--list-engines"}) {
			fmt.Fprintln(os.Stdout, `[{"engine":"fixture","display_name":"Fixture Engine","installed":true,"supported":true},{"engine":"other","display_name":"Other Engine","installed":false,"supported":false}]`)
			os.Exit(0)
		}
		if !reflect.DeepEqual(os.Args[1:], []string{"--resolve", "fixture"}) {
			fmt.Fprintln(os.Stderr, "unexpected resolver arguments")
			os.Exit(2)
		}
		exe, _ := os.Executable()
		path := filepath.Join(filepath.Dir(exe), engineTestBinaryName("fake-engine"))
		if mode == "missing-engine" {
			path += "-missing"
		}
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			Engine string            `json:"engine"`
			Path   string            `json:"path"`
			Env    map[string]string `json:"env"`
		}{"fixture", path, map[string]string{"OLLAMA_HOST": "127.0.0.1:23456", "LD_LIBRARY_PATH": "/fixture/lib"}})
		os.Exit(0)
	case "fake-engine":
		input, _ := io.ReadAll(os.Stdin)
		dir, _ := os.Getwd()
		_ = json.NewEncoder(os.Stdout).Encode(engineObservation{
			Args: os.Args[1:], Input: string(input), Host: os.Getenv("OLLAMA_HOST"),
			Library: os.Getenv("LD_LIBRARY_PATH"), Inherited: os.Getenv("NVPAIR_ENGINE_INHERITED"),
			Path: os.Getenv("PATH"), Dir: dir, PID: os.Getpid(),
		})
		fmt.Fprintln(os.Stderr, "engine stderr")
		if os.Getenv("NVPAIR_ENGINE_TEST_MODE") == "exit-seven" {
			os.Exit(7)
		}
		os.Exit(0)
	}
}

func engineTestBinaryName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func TestEngineCommand(t *testing.T) {
	dir := t.TempDir()
	tui := filepath.Join(dir, engineTestBinaryName("nvpair-tui"))
	build := exec.Command("go", "build", "-o", tui, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build tui: %v\n%s", err, out)
	}
	testExe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(testExe)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"nvpair-engine-manager", "fake-engine", "nvpair-ui-broker"} {
		if err := os.WriteFile(filepath.Join(dir, engineTestBinaryName(name)), data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(dir, "broker-started")
	workdir := t.TempDir()
	run := func(t *testing.T, mode string, args ...string) (int, string, string, int) {
		t.Helper()
		if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, tui, args...)
		cmd.Dir = workdir
		cmd.Env = append(os.Environ(), "NVPAIR_ENGINE_TEST_HELPER=1", "NVPAIR_ENGINE_TEST_MODE="+mode,
			"NVPAIR_ENGINE_BROKER_MARKER="+marker, "NVPAIR_ENGINE_INHERITED=keep me",
			"OLLAMA_HOST=old-host", "LD_LIBRARY_PATH=old-library")
		cmd.Stdin = strings.NewReader("engine input\n")
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if ctx.Err() != nil {
			t.Fatalf("command timed out: %s", stderr.String())
		}
		if err != nil && cmd.ProcessState == nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("engine command started the broker: stat=%v", err)
		}
		return cmd.ProcessState.ExitCode(), stdout.String(), stderr.String(), cmd.Process.Pid
	}

	t.Run("arguments environment stdio cwd and process replacement", func(t *testing.T) {
		args := []string{"--version", "-h", "--", "a b", "", "--broker-path", "not-a-broker"}
		code, out, stderr, pid := run(t, "", append([]string{"engine", "fixture"}, args...)...)
		if code != 0 || stderr != "engine stderr\n" {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, stderr)
		}
		var got engineObservation
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Args, args) {
			t.Errorf("args=%q want %q", got.Args, args)
		}
		if got.Host != "127.0.0.1:23456" || got.Library != "/fixture/lib" || got.Inherited != "keep me" || got.Path != os.Getenv("PATH") {
			t.Errorf("environment: %+v", got)
		}
		resolvedWorkdir, err := filepath.EvalSymlinks(workdir)
		if err != nil {
			t.Fatal(err)
		}
		if got.Input != "engine input\n" || got.Dir != resolvedWorkdir {
			t.Errorf("stdio/cwd: %+v", got)
		}
		if runtime.GOOS != "windows" && got.PID != pid {
			t.Errorf("engine PID=%d want replaced process %d", got.PID, pid)
		}
	})
	t.Run("engine without arguments", func(t *testing.T) {
		code, out, stderr, _ := run(t, "", "engine", "fixture")
		if code != 0 || !strings.Contains(out, `"args":[]`) || stderr != "engine stderr\n" {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, stderr)
		}
	})
	t.Run("exit code", func(t *testing.T) {
		code, _, stderr, _ := run(t, "exit-seven", "engine", "fixture")
		if code != 7 || stderr != "engine stderr\n" {
			t.Fatalf("code=%d stderr=%q", code, stderr)
		}
	})
	t.Run("list", func(t *testing.T) {
		code, out, stderr, _ := run(t, "", "engine")
		if code != 0 || stderr != "" {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, stderr)
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != 5 || strings.Join(strings.Fields(lines[0]), " ") != "ENGINE NAME INSTALLED SUPPORTED" ||
			strings.Join(strings.Fields(lines[1]), " ") != "fixture Fixture Engine true true" ||
			strings.Join(strings.Fields(lines[2]), " ") != "other Other Engine false false" ||
			lines[4] != "nvpair engine <engine> [args...]" {
			t.Fatalf("unexpected table/usage: %q", out)
		}
	})
	t.Run("resolver failure", func(t *testing.T) {
		for _, args := range [][]string{{"engine"}, {"engine", "fixture"}} {
			code, out, stderr, _ := run(t, "resolver-error", args...)
			if code != 2 || out != "" || stderr != "nvpair: fixture is not installed\n" {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, stderr)
			}
		}
	})
	t.Run("invalid JSON", func(t *testing.T) {
		for _, args := range [][]string{{"engine"}, {"engine", "fixture"}} {
			code, out, stderr, _ := run(t, "invalid-json", args...)
			if code != 2 || out != "" || !strings.HasPrefix(stderr, "nvpair: ") || strings.Count(stderr, "\n") != 1 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, stderr)
			}
		}
	})
	t.Run("missing engine executable", func(t *testing.T) {
		code, out, stderr, _ := run(t, "missing-engine", "engine", "fixture")
		if code != 2 || out != "" || !strings.HasPrefix(stderr, "nvpair: ") || strings.Count(stderr, "\n") != 1 {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, stderr)
		}
	})
	t.Run("missing sibling manager", func(t *testing.T) {
		if err := os.Remove(filepath.Join(dir, engineTestBinaryName("nvpair-engine-manager"))); err != nil {
			t.Fatal(err)
		}
		code, out, stderr, _ := run(t, "", "engine", "fixture")
		if code != 2 || out != "" || !strings.Contains(stderr, "nvpair-engine-manager") || !strings.HasPrefix(stderr, "nvpair: ") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, stderr)
		}
	})
}
