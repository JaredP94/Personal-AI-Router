// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func archiveExecutor(t *testing.T) (*Executor, *engineState) {
	t.Helper()
	reg := NewRegistry()
	raw := `{"engine":"archive-fixture","display_name":"Archive fixture","manifest_version":1,"platforms":{"` + runtime.GOOS + `/` + runtime.GOARCH + `":{"detect":["{install_dir}/llama-server"],"install":{"fetch":{"url":"https://example.test/engine.tar.gz","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"archive":{"required":["llama-server"],"codesign":"llama-server"}},"models_dir":"{install_dir}-models","uninstall":{"remove":["{install_dir}"]},"runtime":{"bin":"{install_dir}/llama-server","port":0}}}}`
	if _, err := reg.addManifest("fixture.json", []byte(raw)); err != nil {
		t.Fatal(err)
	}
	ex := reconciledTestExecutor(NewExecutor(reg, NewReporter(nil), nil, t.TempDir()))
	st, err := ex.state("archive-fixture")
	if err != nil {
		t.Fatal(err)
	}
	return ex, st
}

func TestArchiveChecksumMismatch(t *testing.T) {
	ex, st := archiveExecutor(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("wrong archive")) }))
	defer srv.Close()
	st.plat.Install.Fetch.URL = srv.URL + "/engine.tar.gz"
	if err := ex.Install(context.Background(), "archive-fixture"); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(st.installDir); !os.IsNotExist(err) {
		t.Fatal("checksum failure left install directory")
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

func TestArchiveManifestValidation(t *testing.T) {
	for name, change := range map[string]func(*Platform){
		"no checksum":              func(p *Platform) { p.Install.Fetch.SHA256 = "" },
		"bad checksum":             func(p *Platform) { p.Install.Fetch.SHA256 = "not a digest" },
		"script":                   func(p *Platform) { p.Install.Script = []string{"script"} },
		"run":                      func(p *Platform) { p.Install.Run = []string{"tar"} },
		"disk image":               func(p *Platform) { p.Install.DiskImage = &DiskImage{App: "Fixture.app", Into: "/tmp"} },
		"no required":              func(p *Platform) { p.Install.Archive.Required = nil },
		"unsafe required":          func(p *Platform) { p.Install.Archive.Required = []string{"../server"} },
		"missing signature target": func(p *Platform) { p.Install.Archive.Codesign = "other" },
		"unsafe signature target":  func(p *Platform) { p.Install.Archive.Codesign = "../server" },
		"creates":                  func(p *Platform) { p.Install.Creates = "/tmp/other" },
	} {
		t.Run(name, func(t *testing.T) {
			_, st := archiveExecutor(t)
			change(st.plat)
			if err := st.plat.validate(runtime.GOOS + "/" + runtime.GOARCH); err == nil {
				t.Fatal("unsafe archive manifest accepted")
			}
		})
	}
}
