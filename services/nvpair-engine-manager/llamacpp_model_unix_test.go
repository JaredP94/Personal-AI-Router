// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func writeSingleGGUF(t *testing.T, path string) {
	t.Helper()
	data := make([]byte, 24)
	copy(data, "GGUF")
	binary.LittleEndian.PutUint32(data[4:], 3)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestLlamaModelSwapCannotReachOutsideCache(t *testing.T) {
	for _, swapBeforeSpawn := range []bool{true, false} {
		t.Run(map[bool]string{true: "before-spawn", false: "after-spawn"}[swapBeforeSpawn], func(t *testing.T) {
			cache := filepath.Join(t.TempDir(), "llamacpp-models")
			if err := os.Mkdir(cache, 0700); err != nil {
				t.Fatal(err)
			}
			model := filepath.Join(cache, "model.gguf")
			outside := filepath.Join(t.TempDir(), "outside.gguf")
			writeSingleGGUF(t, model)
			writeSingleGGUF(t, outside)
			original, err := os.Stat(model)
			if err != nil {
				t.Fatal(err)
			}
			originalStat, ok := original.Sys().(*syscall.Stat_t)
			if !ok {
				t.Fatal("missing inode")
			}
			args, err := normalizeLlamaOptions([]string{"-m", model}, cache)
			if err != nil {
				t.Fatal(err)
			}
			swap := func() {
				t.Helper()
				if err := os.Rename(model, model+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, model); err != nil {
					t.Fatal(err)
				}
			}
			if swapBeforeSpawn {
				swap()
			}
			gate := filepath.Join(t.TempDir(), "go")
			var mu sync.Mutex
			var lines []string
			proc, err := startManagedProcWithEnvironment(fakeEngineForTest(t), append([]string{"model-inode"}, args...), map[string]string{
				"LLAMA_CACHE": cache, "FAKE_MODEL_GATE": gate, "PAIR_TEST_PROCESS_ROOT": testProcessRoot,
			}, false, func(_, line string) { mu.Lock(); defer mu.Unlock(); lines = append(lines, line) })
			if err != nil {
				if !swapBeforeSpawn {
					t.Fatal(err)
				}
				return // Rejecting the pre-spawn symlink is the expected safe result.
			}
			t.Cleanup(proc.stop)
			if !swapBeforeSpawn {
				swap()
			}
			if err := os.WriteFile(gate, nil, 0600); err != nil {
				t.Fatal(err)
			}
			select {
			case <-proc.done:
			case <-time.After(10 * time.Second):
				t.Fatal("fake engine did not exit")
			}
			mu.Lock()
			defer mu.Unlock()
			if len(lines) != 2 {
				t.Fatalf("expected two model opens, got %v", lines)
			}
			for _, line := range lines {
				var stat syscall.Stat_t
				if err := json.Unmarshal([]byte(line), &stat); err != nil {
					t.Fatal(err)
				}
				if stat.Dev != originalStat.Dev || stat.Ino != originalStat.Ino {
					t.Fatalf("child opened outside inode %d:%d; expected verified inode %d:%d", stat.Dev, stat.Ino, originalStat.Dev, originalStat.Ino)
				}
			}
		})
	}
}

// Re-resolving both paths after this swap used to bless the outside inode.
func TestLlamaModelParentSwapCannotReachOutsideCache(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "parent")
	cache := filepath.Join(parent, "llamacpp-models")
	outside := t.TempDir()
	for _, dir := range []string{cache, filepath.Join(outside, "llamacpp-models")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		writeSingleGGUF(t, filepath.Join(dir, "model.gguf"))
	}
	args, err := normalizeLlamaOptions([]string{"-m", "model.gguf"}, cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(parent, parent+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, parent); err != nil {
		t.Fatal(err)
	}
	proc, err := startManagedProcWithEnvironment(fakeEngineForTest(t), append([]string{"model-inode"}, args...), map[string]string{
		"LLAMA_CACHE": cache, "PAIR_TEST_PROCESS_ROOT": testProcessRoot,
	}, false, nil)
	if proc != nil {
		proc.stop()
	}
	if err == nil {
		t.Fatal("started with a model redirected through a swapped cache parent")
	}
}

func TestLlamaModelCanonicalParentSwapRejected(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "parent")
	cache := filepath.Join(parent, "llamacpp-models")
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(cache)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(outside, "llamacpp-models"), 0700); err != nil {
		t.Fatal(err)
	}
	writeSingleGGUF(t, filepath.Join(outside, "llamacpp-models", "model.gguf"))
	if err := os.Rename(parent, parent+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, parent); err != nil {
		t.Fatal(err)
	}
	file, err := openLlamaModelAt(root, "model.gguf")
	if file != nil {
		_ = file.Close()
	}
	// The directory walk must fail, before opening even the outside model.
	if err == nil || !strings.Contains(err.Error(), "directory changed") {
		t.Fatalf("canonical cache parent swap not rejected by directory walk: %v", err)
	}
}

func TestLlamaModelSubdirectoryLaunchClosesDescriptors(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "llamacpp-models")
	nested := filepath.Join(cache, "nested")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(nested, "model.gguf")
	writeSingleGGUF(t, model)
	original, err := os.Stat(model)
	if err != nil {
		t.Fatal(err)
	}
	originalStat, ok := original.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("missing inode")
	}
	args, err := normalizeLlamaOptions([]string{"-m", "nested/model.gguf"}, cache)
	if err != nil {
		t.Fatal(err)
	}
	bin := fakeEngineForTest(t)
	launch := func(bin string, wantSuccess bool) {
		t.Helper()
		var mu sync.Mutex
		var lines []string
		proc, err := startManagedProcWithEnvironment(bin, append([]string{"model-inode"}, args...), map[string]string{
			"LLAMA_CACHE": cache, "PAIR_TEST_PROCESS_ROOT": testProcessRoot,
		}, false, func(_, line string) { mu.Lock(); defer mu.Unlock(); lines = append(lines, line) })
		if !wantSuccess {
			if proc != nil {
				proc.stop()
			}
			if err == nil {
				t.Fatal("invalid launch succeeded")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(proc.stop)
		select {
		case <-proc.done:
		case <-time.After(10 * time.Second):
			t.Fatal("fake engine did not exit")
		}
		mu.Lock()
		defer mu.Unlock()
		if len(lines) != 2 {
			t.Fatalf("expected two nested model opens, got %v", lines)
		}
		for _, line := range lines {
			var stat syscall.Stat_t
			if err := json.Unmarshal([]byte(line), &stat); err != nil {
				t.Fatal(err)
			}
			if stat.Dev != originalStat.Dev || stat.Ino != originalStat.Ino {
				t.Fatal("child did not open the selected nested model")
			}
		}
	}
	countFDs := func() int {
		t.Helper()
		dir, err := os.Open("/dev/fd")
		if err != nil {
			t.Fatal(err)
		}
		// Descriptor entries may disappear during enumeration. Read names only:
		// ReadDir can stat such an entry and fail with EBADF on macOS.
		names, readErr := dir.Readdirnames(-1)
		closeErr := dir.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		return len(names)
	}
	launch(bin, true) // Initialize the runtime's poller before the baseline.
	before := countFDs()
	for range 3 {
		launch(bin, true)
		launch(filepath.Join(t.TempDir(), "missing-engine"), false)
	}
	if err := os.Rename(nested, nested+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), nested); err != nil {
		t.Fatal(err)
	}
	launch(bin, false) // Fail after opening cache ancestors, not just at exec.
	if after := countFDs(); after != before {
		t.Fatalf("launch leaked descriptors: before=%d after=%d", before, after)
	}
}

func TestLlamaModelRelativeTraversalRejected(t *testing.T) {
	cache := t.TempDir()
	writeSingleGGUF(t, filepath.Join(cache, "model.gguf"))
	for _, rel := range []string{"../model.gguf", "nested/../model.gguf", "/model.gguf", "./model.gguf"} {
		file, err := openLlamaModelAt(cache, rel)
		if file != nil {
			_ = file.Close()
		}
		if err == nil {
			t.Errorf("accepted non-relative or traversing model path %q", rel)
		}
	}
}

func TestLlamaSplitModelRejectedAtSpawn(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "llamacpp-models")
	if err := os.Mkdir(cache, 0700); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(cache, "renamed.gguf")
	writeSingleGGUF(t, model)
	file, err := os.OpenFile(model, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	// A renamed shard must be rejected too: split.count is GGUF uint16 (type 2).
	_, _ = file.Seek(16, 0)
	for _, v := range []interface{}{uint64(1), uint64(len("split.count")), []byte("split.count"), uint32(2), uint16(2)} {
		if err := binary.Write(file, binary.LittleEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	_ = file.Close()
	// As in the production launch path, pass the canonical validated choice.
	args, err := normalizeLlamaOptions([]string{"--model", model}, cache)
	if err != nil {
		t.Fatal(err)
	}
	proc, err := startManagedProcWithEnvironment(fakeEngineForTest(t), append([]string{"model-inode"}, args...), map[string]string{"LLAMA_CACHE": cache, "PAIR_TEST_PROCESS_ROOT": testProcessRoot}, false, nil)
	if proc != nil {
		proc.stop()
	}
	if err == nil || !strings.Contains(err.Error(), "split") {
		t.Fatalf("split model accepted: %v", err)
	}
}
