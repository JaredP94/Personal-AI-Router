// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLlamaDownloadTempSwapAtCommit(t *testing.T) {
	ex, cache := llamaDownloadExecutor(t, nil, nil)
	ex.llamaCommit = func(dir *os.File, from, to string) error {
		path := filepath.Join(cache, from)
		if err := os.Rename(path, path+".saved"); err != nil {
			return err
		}
		if err := os.WriteFile(path, llamaTestBytes(), 0600); err != nil {
			return err
		}
		return llamaCacheCommit(dir, from, to)
	}
	if err := pullLlamaTest(ex, "temp-swap"); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("swap accepted: %v", err)
	}
	dir, err := openLlamaCache(cache, false)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	manifest, err := readLlamaManifest(dir)
	if err != nil || len(manifest.Models) != 0 {
		t.Fatalf("claimed replacement: %+v %v", manifest, err)
	}
	final := filepath.Join(cache, llamaOwnedFilename(llamaTestID))
	if _, err := os.Stat(final); err != nil {
		t.Fatalf("replacement removed: %v", err)
	}
	data, err := ex.listLlamaModels(context.Background())
	if err != nil || strings.Contains(string(data), ".gguf") {
		t.Fatalf("replacement selectable: %s %v", data, err)
	}
	if _, err := llamaModelPath(cache, final); err == nil {
		t.Fatal("unrecorded managed replacement accepted for selection")
	}
	if f, err := openLlamaModel(cache, final); err == nil {
		f.Close()
		t.Fatal("unrecorded managed replacement accepted for launch")
	}
}

func TestLlamaRecordedReplacementRefusedAtLaunch(t *testing.T) {
	ex, cache := llamaDownloadExecutor(t, nil, nil)
	if err := pullLlamaTest(ex, "launch-swap"); err != nil {
		t.Fatal(err)
	}
	name := llamaOwnedFilename(llamaTestID)
	path := filepath.Join(cache, name)
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, llamaTestBytes(), 0600); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{llamaTestID, name, path} {
		t.Run(value, func(t *testing.T) {
			if _, err := llamaModelPath(cache, value); err == nil {
				t.Fatal("replacement accepted for selection")
			}
			if f, err := openLlamaModel(cache, value); err == nil {
				f.Close()
				t.Fatal("replacement accepted for launch")
			}
		})
	}
}
