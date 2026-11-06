// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type archiveEntry struct {
	name, link, body string
	kind             byte
	mode             int64
	pax              map[string]string
}

func archiveFixture(t *testing.T, entries ...archiveEntry) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "engine.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		kind := e.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0755
		}
		h := &tar.Header{Name: e.name, Linkname: e.link, Typeflag: kind, Mode: mode, PAXRecords: e.pax}
		if kind == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if kind == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, close := range []func() error{tw.Close, gz.Close, f.Close} {
		if err := close(); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func TestArchiveContainment(t *testing.T) {
	cases := map[string][]archiveEntry{
		"absolute":            {{name: "/outside"}},
		"parent":              {{name: "bundle/../outside"}},
		"symlink escape":      {{name: "bundle/link", kind: tar.TypeSymlink, link: "../../outside"}},
		"hardlink escape":     {{name: "bundle/link", kind: tar.TypeLink, link: "../outside"}},
		"absolute symlink":    {{name: "bundle/link", kind: tar.TypeSymlink, link: "/tmp/outside"}},
		"absolute hardlink":   {{name: "bundle/link", kind: tar.TypeLink, link: "/tmp/outside"}},
		"device":              {{name: "bundle/device", kind: tar.TypeChar}},
		"block":               {{name: "bundle/device", kind: tar.TypeBlock}},
		"fifo":                {{name: "bundle/pipe", kind: tar.TypeFifo}},
		"setuid":              {{name: "bundle/server", mode: 04755}},
		"setgid":              {{name: "bundle/server", mode: 02755}},
		"duplicate":           {{name: "bundle/server"}, {name: "bundle/server"}},
		"case alias":          {{name: "bundle/server"}, {name: "bundle/SERVER"}},
		"link parent":         {{name: "bundle/link", kind: tar.TypeSymlink, link: "dir"}, {name: "bundle/link/evil"}},
		"cycle":               {{name: "bundle/a", kind: tar.TypeSymlink, link: "b"}, {name: "bundle/b", kind: tar.TypeSymlink, link: "a"}},
		"quarantine metadata": {{name: "bundle/server", pax: map[string]string{"SCHILY.xattr.com.apple.quarantine": "fixture"}}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := extractTarGzip(context.Background(), archiveFixture(t, entries...), root, 1024, 20); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
}

func TestArchiveStripsWrapperAndPreservesInternalLinks(t *testing.T) {
	root := t.TempDir()
	p := archiveFixture(t, archiveEntry{name: "bundle", kind: tar.TypeDir}, archiveEntry{name: "bundle/lib.dylib", body: "signed bytes"}, archiveEntry{name: "bundle/lib.0.dylib", kind: tar.TypeSymlink, link: "lib.dylib"}, archiveEntry{name: "bundle/copy", kind: tar.TypeLink, link: "bundle/lib.dylib"})
	if err := extractTarGzip(context.Background(), p, root, 1024, 20); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"lib.dylib", "lib.0.dylib", "copy"} {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(b) != "signed bytes" {
			t.Fatalf("%s: %q %v", name, b, err)
		}
	}
}

func TestArchiveLimitsAndCancellation(t *testing.T) {
	p := archiveFixture(t, archiveEntry{name: "bundle/a", body: "12345"}, archiveEntry{name: "bundle/b", body: "67890"})
	for _, limit := range []struct {
		bytes int64
		files int
	}{{9, 20}, {100, 1}} {
		if err := extractTarGzip(context.Background(), p, t.TempDir(), limit.bytes, limit.files); err == nil {
			t.Fatal("archive limit ignored")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := extractTarGzip(ctx, p, t.TempDir(), 100, 20); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func acceptArchive(context.Context, string, *ArchiveInstall) error { return nil }

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

func TestArchiveRefusesUnownedReplacement(t *testing.T) {
	target := filepath.Join(t.TempDir(), "engine")
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatal(err)
	}
	p := archiveFixture(t, archiveEntry{name: "bundle/llama-server", body: "new"})
	if err := installArchive(context.Background(), p, target, "fixture", "digest", &ArchiveInstall{Required: []string{"llama-server"}, Codesign: "llama-server"}, acceptArchive); err == nil || !strings.Contains(err.Error(), "PAIR") {
		t.Fatalf("unowned replacement: %v", err)
	}
}
