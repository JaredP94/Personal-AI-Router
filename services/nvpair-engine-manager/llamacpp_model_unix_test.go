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
	proc, err := startManagedProcWithEnvironment(fakeEngineForTest(t), []string{"model-inode", "--model", model}, map[string]string{"LLAMA_CACHE": cache, "PAIR_TEST_PROCESS_ROOT": testProcessRoot}, false, nil)
	if proc != nil {
		proc.stop()
	}
	if err == nil || !strings.Contains(err.Error(), "split") {
		t.Fatalf("split model accepted: %v", err)
	}
}
