// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLlamaDeleteParentSwapCannotEscape(t *testing.T) {
	ex, cache := llamaDownloadExecutor(t, nil, nil)
	if err := pullLlamaTest(ex, "download"); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(cache)
	outside := t.TempDir()
	outsideCache := filepath.Join(outside, "llamacpp-models")
	if err := os.Mkdir(outsideCache, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{llamaManifestName, llamaOwnedFilename(llamaTestID)} {
		data, err := os.ReadFile(filepath.Join(cache, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outsideCache, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Rename(parent, parent+".saved"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(parent); _ = os.Rename(parent+".saved", parent) })
	if err := os.Symlink(outside, parent); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.ModelDelete(context.Background(), "llamacpp", llamaTestID); err == nil {
		t.Fatal("swapped parent was accepted")
	}
	if _, err := os.Stat(filepath.Join(outsideCache, llamaOwnedFilename(llamaTestID))); err != nil {
		t.Fatal("outside file changed")
	}
}
func TestLlamaDeleteRefusesReplacedRecordedFile(t *testing.T) {
	ex, cache := llamaDownloadExecutor(t, nil, nil)
	if err := pullLlamaTest(ex, "original"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cache, llamaOwnedFilename(llamaTestID))
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, llamaTestBytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.ModelDelete(context.Background(), "llamacpp", llamaTestID); err == nil {
		t.Fatal("deleted replacement that PAIR did not record")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("replacement disappeared")
	}
}

func TestLlamaOwnershipManifestEntryBound(t *testing.T) {
	dir, err := openLlamaCache(filepath.Join(t.TempDir(), "cache"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := writeLlamaManifest(dir, llamaCacheManifest{Models: make([]llamaCacheRecord, 4097)}); err == nil {
		t.Fatal("wrote a manifest the reader must reject")
	}
	if _, err := llamaCacheOpen(dir, llamaManifestName, false); !os.IsNotExist(err) {
		t.Fatal("oversized manifest was committed")
	}
}

func TestLlamaCommittedDescriptorDoesNotClaimReplacement(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "cache")
	dir, err := openLlamaCache(cache, true)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	verified, err := llamaCacheOpen(dir, "temp", true)
	if err != nil {
		t.Fatal(err)
	}
	defer verified.Close()
	if _, err := verified.Write(llamaTestBytes()); err != nil {
		t.Fatal(err)
	}
	if err := llamaCacheCommit(dir, "temp", "model.gguf"); err != nil {
		t.Fatal(err)
	}
	original := llamaCacheIdentity(verified)
	if original == "" {
		t.Fatal("missing committed identity")
	}
	if err := os.Rename(filepath.Join(cache, "model.gguf"), filepath.Join(cache, "saved.gguf")); err != nil {
		t.Fatal(err)
	}
	replacement, err := llamaCacheOpen(dir, "model.gguf", true)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if llamaCacheIdentity(verified) == llamaCacheIdentity(replacement) {
		t.Fatal("replacement acquired verified descriptor identity")
	}
}

func TestLlamaCachePinnedDirectoryAndNoClobber(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "models")
	dir, err := openLlamaCache(cache, true)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := os.Rename(cache, cache+".saved"); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "model.gguf"), []byte("outside"), 0600)
	if err := os.Symlink(outside, cache); err != nil {
		t.Fatal(err)
	}
	file, err := llamaCacheOpen(dir, "model.gguf", true)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	if err := llamaCacheRemove(dir, "model.gguf"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(outside, "model.gguf"))
	if string(data) != "outside" {
		t.Fatal("escaped pinned directory")
	}
	for _, name := range []string{"temp", "existing.gguf"} {
		f, err := llamaCacheOpen(dir, name, true)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	if err := llamaCacheCommit(dir, "temp", "existing.gguf"); err == nil {
		t.Fatal("overwrote an existing file")
	}
}
