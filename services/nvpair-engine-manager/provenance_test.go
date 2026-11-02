// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestInstallMarkerRoundTrip(t *testing.T) {
	installDir := filepath.Join(t.TempDir(), "lmstudio")
	if installedByPAIR(installDir) {
		t.Fatal("an install directory that does not exist cannot be ours")
	}
	if err := writeInstallMarker(installDir, "lmstudio"); err != nil {
		t.Fatalf("writeInstallMarker: %v", err)
	}
	if !installedByPAIR(installDir) {
		t.Error("marker written but not recognized")
	}
	clearInstallMarker(installDir)
	if installedByPAIR(installDir) {
		t.Error("marker survived clearInstallMarker; a stale claim lets PAIR remove a user's own install")
	}
}

// vendorEngineManifest describes a command-mode engine whose CLI and model
// store share a vendor home outside the install directory — the shape of LM
// Studio's ~/.lmstudio and oMLX's ~/.omlx, which the install marker and model
// preservation exist for.
func vendorEngineManifest(vendorRoot string, remove []string) *Manifest {
	ok := []string{"true"}
	if runtime.GOOS == "windows" {
		ok = []string{"cmd", "/c", "exit", "0"}
	}
	return &Manifest{
		Engine:          "vendor",
		DisplayName:     "Vendor Engine",
		ManifestVersion: 1,
		Platforms: map[string]Platform{
			runtime.GOOS + "/" + runtime.GOARCH: {
				Detect:    []string{filepath.Join(vendorRoot, "bin", "engine")},
				ModelsDir: filepath.Join(vendorRoot, "models"),
				Uninstall: &Uninstall{
					CLIRoots: []string{filepath.Join(vendorRoot, "bin")},
					Remove:   remove,
				},
				Runtime: Runtime{
					Mode:  "command",
					Port:  45999,
					Start: [][]string{ok},
					Stop:  &StopSpec{Cmd: ok},
				},
			},
		},
	}
}

// vendorEngineOnDisk lays down an engine binary, a downloaded model and a
// settings file the user owns, and returns all three paths.
func vendorEngineOnDisk(t *testing.T, vendorRoot string) (binary, weights, settings string) {
	t.Helper()
	binary = filepath.Join(vendorRoot, "bin", "engine")
	weights = filepath.Join(vendorRoot, "models", "publisher", "model.gguf")
	settings = filepath.Join(vendorRoot, "settings.json")
	for _, file := range []string{binary, weights, settings} {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return binary, weights, settings
}

func vendorExecutor(t *testing.T, manifest *Manifest) *Executor {
	t.Helper()
	ex := newTestExecutor(t, manifest)
	ex.detectTimeout = 100 * time.Millisecond
	return ex
}

// A command-mode engine the user installed themselves has its CLI exactly
// where a PAIR install would put it, so only the marker separates the two.
// Without it, uninstall declines and touches nothing, and the capability is
// not offered.
func TestUninstallDeclinesUnmarkedCommandEngine(t *testing.T) {
	vendorRoot := t.TempDir()
	binary, weights, settings := vendorEngineOnDisk(t, vendorRoot)
	ex := vendorExecutor(t, vendorEngineManifest(vendorRoot, []string{filepath.Join(vendorRoot, "bin")}))

	status, err := ex.Status("vendor")
	if err != nil || !status.Installed || status.Capabilities.Uninstall {
		t.Errorf("status = %+v, %v; want installed with uninstall not offered", status, err)
	}
	if err := ex.Uninstall(context.Background(), "vendor"); err == nil {
		t.Fatal("expected uninstall to decline an engine PAIR has no record of installing")
	}
	for _, path := range []string{binary, weights, settings} {
		if !fileExists(path) {
			t.Errorf("declined uninstall still removed %s", path)
		}
	}
}

// An engine PAIR installed is removed, and its model store and the user's
// settings beside it survive.
func TestUninstallRemovesAMarkedEngineAndKeepsItsData(t *testing.T) {
	vendorRoot := t.TempDir()
	binary, weights, settings := vendorEngineOnDisk(t, vendorRoot)
	ex := vendorExecutor(t, vendorEngineManifest(vendorRoot, []string{filepath.Join(vendorRoot, "bin")}))
	installDir := filepath.Join(ex.baseDir, "vendor")
	if err := writeInstallMarker(installDir, "vendor"); err != nil {
		t.Fatal(err)
	}

	status, err := ex.Status("vendor")
	if err != nil || !status.Capabilities.Uninstall {
		t.Errorf("status = %+v, %v; want uninstall offered for a PAIR install", status, err)
	}
	if err := ex.Uninstall(context.Background(), "vendor"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if fileExists(binary) {
		t.Error("engine binary survived uninstall")
	}
	if !fileExists(weights) {
		t.Error("uninstall deleted the user's downloaded model")
	}
	if !fileExists(settings) {
		t.Error("uninstall deleted the user's settings")
	}
	if installedByPAIR(installDir) {
		t.Error("marker survived a completed uninstall")
	}
}

// TestUninstallClearsTheMarkerWhenAlreadyGone covers the engine removed by its
// own uninstaller. Leaving the claim behind would authorise removing a copy the
// user installs later.
func TestUninstallClearsTheMarkerWhenAlreadyGone(t *testing.T) {
	vendorRoot := t.TempDir() // nothing laid down, so detection fails
	ex := vendorExecutor(t, vendorEngineManifest(vendorRoot, []string{filepath.Join(vendorRoot, "bin")}))
	installDir := filepath.Join(ex.baseDir, "vendor")
	if err := writeInstallMarker(installDir, "vendor"); err != nil {
		t.Fatal(err)
	}

	if err := ex.Uninstall(context.Background(), "vendor"); err != nil {
		t.Fatalf("uninstalling an absent engine should succeed: %v", err)
	}
	if installedByPAIR(installDir) {
		t.Error("stale marker left behind for an engine that is already gone")
	}
}
