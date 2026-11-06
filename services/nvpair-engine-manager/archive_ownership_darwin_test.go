// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Successful directory claims and atomic updates are macOS-only: birth time
// survives writing the marker and renaming the staging directory. Linux's
// existing ctime identity deliberately invalidates such a claim; the runtime
// installer fails closed there. Portable extraction and validation stay in the
// platform-independent test files.
func TestArchiveInstallRollback(t *testing.T) {
	target := filepath.Join(t.TempDir(), "engine")
	spec := &ArchiveInstall{Required: []string{"llama-server", "lib.dylib"}, Codesign: "llama-server"}
	good := archiveFixture(t, archiveEntry{name: "bundle/llama-server", body: "old"}, archiveEntry{name: "bundle/lib.dylib", body: "metal"})
	bad := archiveFixture(t, archiveEntry{name: "bundle/llama-server", body: "partial"})
	if err := installArchive(context.Background(), bad, target, "fixture", "old-digest", spec, acceptArchive); err == nil {
		t.Fatal("partial install succeeded")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("partial install left target: %v", err)
	}
	if err := installArchive(context.Background(), good, target, "fixture", "old-digest", spec, acceptArchive); err != nil {
		t.Fatal(err)
	}
	if !installedByPAIR(target, target) {
		t.Fatal("missing identity-bound claim")
	}
	for _, attempt := range []struct {
		archive string
		verify  func(context.Context, string, *ArchiveInstall) error
	}{
		{bad, acceptArchive}, {good, func(context.Context, string, *ArchiveInstall) error { return errors.New("broken signature") }},
	} {
		if err := installArchive(context.Background(), attempt.archive, target, "fixture", "new-digest", spec, attempt.verify); err == nil {
			t.Fatal("failed update succeeded")
		}
		b, err := os.ReadFile(filepath.Join(target, "llama-server"))
		if err != nil || string(b) != "old" || !installedByPAIR(target, target) {
			t.Fatal("update did not preserve old installation and claim")
		}
	}
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil || len(entries) != 1 {
		t.Fatalf("staging debris: %v %v", entries, err)
	}
}

func TestArchiveUninstallPreservesModelCacheAndRequiresClaim(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(map[bool]string{false: "unowned", true: "owned"}[owned], func(t *testing.T) {
			ex, st := archiveExecutor(t)
			p := archiveFixture(t, archiveEntry{name: "bundle/llama-server", body: "fixture"})
			if err := installArchive(context.Background(), p, st.installDir, "archive-fixture", "digest", st.plat.Install.Archive, acceptArchive); err != nil {
				t.Fatal(err)
			}
			if !owned {
				clearInstallMarker(st.installDir)
			}
			cache := st.installDir + "-models"
			if err := os.MkdirAll(cache, 0755); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(cache, "keep.gguf")
			if err := os.WriteFile(sentinel, []byte("model"), 0644); err != nil {
				t.Fatal(err)
			}
			err := ex.Uninstall(context.Background(), "archive-fixture")
			if owned && err != nil {
				t.Fatal(err)
			}
			if !owned && err == nil {
				t.Fatal("unclaimed installation removed")
			}
			if !fileExists(sentinel) {
				t.Fatal("model cache deleted")
			}
			if fileExists(filepath.Join(st.installDir, "llama-server")) == owned {
				t.Fatal("wrong uninstall result")
			}
		})
	}
}
