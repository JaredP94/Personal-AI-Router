// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// Copy an already signed system fixture; never sign or execute it.
func signedArchiveFixture(t *testing.T) (string, *ArchiveInstall) {
	t.Helper()
	root := t.TempDir()
	b, err := os.ReadFile("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "llama-server"), b, 0755); err != nil {
		t.Fatal(err)
	}
	return root, &ArchiveInstall{Required: []string{"llama-server"}, Codesign: "llama-server"}
}

func TestArchiveSignatureIntegrity(t *testing.T) {
	root, spec := signedArchiveFixture(t)
	if err := verifyArchivePlatform(context.Background(), root, spec); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "llama-server")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)/2] ^= 0xff
	if err := os.WriteFile(p, b, 0755); err != nil {
		t.Fatal(err)
	}
	if err := verifyArchivePlatform(context.Background(), root, spec); err == nil {
		t.Fatal("broken signature accepted")
	}
}

func TestArchiveCodesignUnavailable(t *testing.T) {
	root, spec := signedArchiveFixture(t)
	if err := verifyArchiveSignature(context.Background(), root, spec, filepath.Join(root, "missing-codesign")); err == nil {
		t.Fatal("missing codesign accepted")
	}
}

func TestArchiveQuarantinePreservedAndRejected(t *testing.T) {
	root, spec := signedArchiveFixture(t)
	p := filepath.Join(root, "metal-resource")
	if err := os.WriteFile(p, []byte("fixture"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(p, "com.apple.quarantine", []byte("0081;fixture;PAIR;"), 0); err != nil {
		t.Fatal(err)
	}
	err := verifyArchivePlatform(context.Background(), root, spec)
	if err == nil || !strings.Contains(err.Error(), "metal-resource") {
		t.Fatalf("quarantine not identified: %v", err)
	}
	if _, err := unix.Getxattr(p, "com.apple.quarantine", nil); err != nil {
		t.Fatalf("quarantine was changed: %v", err)
	}
}

func TestArchiveAtomicUpdate(t *testing.T) {
	target := filepath.Join(t.TempDir(), "engine")
	spec := &ArchiveInstall{Required: []string{"llama-server"}, Codesign: "llama-server"}
	for _, version := range []string{"first", "updated"} {
		p := archiveFixture(t, archiveEntry{name: "bundle/llama-server", body: version})
		if err := installArchive(context.Background(), p, target, "fixture", version, spec, acceptArchive); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(target, "llama-server"))
		if err != nil || string(b) != version || !installedByPAIR(target, target) {
			t.Fatalf("update: %q %v", b, err)
		}
	}
}
