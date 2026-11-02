// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// Ownership must hold both as written and resolved, so a link at either end
// cannot make an external binary look like PAIR's.
func TestIsOwnedInstallPathRejectsLinksOut(t *testing.T) {
	base := t.TempDir()
	external := filepath.Join(base, "Applications", "Ollama.app", "ollama")
	writeExecutable(t, external)

	// An install directory that is itself a link to the external location.
	linkedDir := filepath.Join(base, "engine-bin", "ollama")
	if err := os.MkdirAll(filepath.Dir(linkedDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "Applications"), linkedDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if isOwnedInstallPath(external, linkedDir) {
		t.Error("an external binary was owned through an install directory that links to it")
	}

	// A binary inside a real install directory that links out.
	realDir := filepath.Join(base, "engine-bin", "other")
	linkedBin := filepath.Join(realDir, "ollama")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, linkedBin); err != nil {
		t.Fatal(err)
	}
	if isOwnedInstallPath(linkedBin, realDir) {
		t.Error("a binary linking out of the install directory was owned")
	}

	// A real binary in a real install directory, however the temp root is
	// spelled (on macOS it is reached through /var -> /private/var).
	owned := filepath.Join(realDir, "Ollama.app", "ollama")
	writeExecutable(t, owned)
	if !isOwnedInstallPath(owned, realDir) {
		t.Error("a binary inside the install directory was not owned")
	}
	if resolved, err := filepath.EvalSymlinks(owned); err == nil && !isOurEngineImage(resolved, owned) {
		t.Error("the resolved running image did not match the configured path")
	}
}
