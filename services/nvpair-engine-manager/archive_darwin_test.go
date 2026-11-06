// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

func TestArchiveQuarantinedSourceRejected(t *testing.T) {
	p := archiveFixture(t, archiveEntry{name: "bundle/llama-server", body: "fixture"})
	if err := unix.Setxattr(p, "com.apple.quarantine", []byte("0081;fixture;PAIR;"), 0); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "engine")
	if err := installArchive(context.Background(), p, target, "fixture", "digest", &ArchiveInstall{Required: []string{"llama-server"}, Codesign: "llama-server"}, acceptArchive); err == nil {
		t.Fatal("quarantined source accepted")
	}
	if _, err := unix.Getxattr(p, "com.apple.quarantine", nil); err != nil {
		t.Fatal("quarantine was altered")
	}
}

func TestArchiveExecutorInstallUpdateRollback(t *testing.T) {
	ex, st := archiveExecutor(t)
	st.plat.Install.Archive.Required = []string{"llama-server", "lib.dylib"}
	signed, err := os.ReadFile("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	install := func(server []byte, library string) error {
		p := archiveFixture(t, archiveEntry{name: "bundle/llama-server", body: string(server)}, archiveEntry{name: "bundle/lib.dylib", body: library})
		payload, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(payload)
		st.plat.Install.Fetch.SHA256 = hex.EncodeToString(sum[:])
		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); _, _ = w.Write(payload) }))
		defer srv.Close()
		st.plat.Install.Fetch.URL = srv.URL + "/fixture.tar.gz"
		if err := ex.Install(context.Background(), "archive-fixture"); err != nil {
			return err
		}
		if err := ex.Install(context.Background(), "archive-fixture"); err != nil {
			return err
		}
		if requests.Load() != 1 {
			t.Fatal("unchanged pin downloaded again")
		}
		return nil
	}
	if err := install([]byte("broken signature"), "old"); err == nil {
		t.Fatal("broken initial signature accepted")
	}
	if _, err := os.Stat(st.installDir); !os.IsNotExist(err) {
		t.Fatal("broken initial install was published")
	}
	if err := install(signed, "old"); err != nil {
		t.Fatal(err)
	}
	for _, attempt := range []struct {
		server  []byte
		library string
	}{{signed, ""}, {[]byte("broken signature"), "new"}} {
		if err := install(attempt.server, attempt.library); err == nil {
			t.Fatal("invalid update accepted")
		}
		body, err := os.ReadFile(filepath.Join(st.installDir, "lib.dylib"))
		if err != nil || string(body) != "old" || !installedByPAIR(st.installDir, st.installDir) {
			t.Fatal("old install was not restored")
		}
	}
	if err := install(signed, "updated"); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(st.installDir, "lib.dylib"))
	if err != nil || string(body) != "updated" {
		t.Fatalf("update not published: %q %v", body, err)
	}
	if err := ex.Uninstall(context.Background(), "archive-fixture"); err != nil {
		t.Fatal(err)
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
