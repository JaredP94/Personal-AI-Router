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

func TestFetchForPicksTheNewestVariantTheHostMeets(t *testing.T) {
	inst := &Install{FetchVariants: []Fetch{
		{URL: "tahoe", MinOSVersion: 26},
		{URL: "sequoia", MinOSVersion: 15},
	}}
	for osMajor, want := range map[int]string{27: "tahoe", 26: "tahoe", 15: "sequoia", 14: "", 0: ""} {
		got := ""
		if f := inst.fetchFor(osMajor); f != nil {
			got = f.URL
		}
		if got != want {
			t.Errorf("fetchFor(%d) = %q, want %q", osMajor, got, want)
		}
	}
	single := &Install{Fetch: &Fetch{URL: "only"}}
	if f := single.fetchFor(0); f == nil || f.URL != "only" {
		t.Errorf("a single fetch should apply on any host, got %+v", f)
	}
}

// oMLX installs the way oMLX recommends: its release disk image, pinned by
// checksum, copied to /Applications. Uninstall removes the app and the shims
// it writes, never the user's models, settings or other files in ~/.omlx.
func TestBundledOMLXInstallsFromItsReleaseDiskImage(t *testing.T) {
	m, ok := bundledManifestSet(t)["omlx"]
	if !ok {
		t.Fatal("no bundled omlx manifest")
	}
	p, ok := m.Platforms["darwin/arm64"]
	if !ok || p.Install == nil || p.Uninstall == nil {
		t.Fatalf("omlx darwin/arm64 install/uninstall missing: %+v", p)
	}
	if len(p.Install.FetchVariants) != 2 {
		t.Fatalf("fetch variants = %+v, want the macOS 26 and macOS 15 builds", p.Install.FetchVariants)
	}
	for _, v := range p.Install.FetchVariants {
		if len(v.SHA256) != 64 || filepath.Ext(v.URL) != ".dmg" {
			t.Errorf("variant %+v is not a checksum-pinned disk image", v)
		}
	}
	if p.Install.Creates != "/Applications/oMLX.app" {
		t.Errorf("creates = %q", p.Install.Creates)
	}
	if di := p.Install.DiskImage; di == nil || di.App != "oMLX.app" || di.Into != "/Applications" || !di.Open {
		t.Errorf("disk_image = %+v, want oMLX.app copied to /Applications and opened", di)
	}
	// No shell program: argv runs through placeholder and environment
	// expansion, which would rewrite a script's own $variables.
	if len(p.Install.Run) != 0 {
		t.Errorf("install.run = %q, want the disk_image step alone", p.Install.Run)
	}
	for _, target := range p.Uninstall.Remove {
		clean := filepath.Clean(expandPath(target))
		if clean == filepath.Clean(expandPath("~/.omlx/bin")) || clean == filepath.Clean(expandPath("~/.omlx")) {
			t.Errorf("uninstall removes %s, which holds files the app did not write", target)
		}
	}
}

// PAIR claims an install only if it put the engine in place. Finding the
// user's own copy already there claims nothing, so uninstall declines it.
func TestInstallClaimsOnlyWhatItCreates(t *testing.T) {
	for _, preexisting := range []bool{false, true} {
		dir := t.TempDir()
		app := filepath.Join(dir, "Engine.app")
		cli := filepath.Join(dir, "bin", "engine")
		if preexisting {
			if err := os.MkdirAll(app, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		script := []string{"sh", "-c", "mkdir -p " + filepath.Dir(cli) + " " + app + " && touch " + cli}
		if runtime.GOOS == "windows" {
			t.Skip("sh")
		}
		m := &Manifest{
			Engine: "vendor", DisplayName: "Vendor", ManifestVersion: 1,
			Platforms: map[string]Platform{runtime.GOOS + "/" + runtime.GOARCH: {
				Detect:  []string{cli},
				Install: &Install{Script: script, Creates: app},
				Runtime: Runtime{Mode: "command", Start: [][]string{{"true"}}},
			}},
		}
		ex := newTestExecutor(t, m)
		ex.detectTimeout = time.Second
		if err := ex.Install(context.Background(), "vendor"); err != nil {
			t.Fatalf("install (preexisting=%v): %v", preexisting, err)
		}
		st, _ := ex.state("vendor")
		if got := installedByPAIR(st.installDir, app); got == preexisting {
			t.Errorf("preexisting=%v: installedByPAIR = %v", preexisting, got)
		}
	}
}

// A claim that names what the install created covers that exact file only.
// A copy put there later is a different file, so the claim lapses.
func TestInstallClaimFollowsTheCreatedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		// No file identity there: a claim naming what it created is refused.
		if err := writeInstallMarker(t.TempDir(), "vendor", t.TempDir(), ""); err == nil {
			t.Fatal("a claim was recorded without a file identity")
		}
		return
	}
	dir := t.TempDir()
	app := filepath.Join(dir, "Engine.app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	installDir := filepath.Join(dir, "install")
	if err := writeInstallMarker(installDir, "vendor", app, fileIdentity(app)); err != nil {
		t.Fatal(err)
	}
	if !installedByPAIR(installDir, app) {
		t.Fatal("the claim did not cover the app it was written for")
	}
	if err := os.RemoveAll(app); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	if installedByPAIR(installDir, app) {
		t.Fatal("the claim covered an app put in place after the install")
	}
}

// An install that finds the user's copy in place drops any claim left from an
// earlier install, so it cannot cover that copy.
func TestInstallThatFindsACopyDropsAStaleClaim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh")
	}
	dir := t.TempDir()
	app := filepath.Join(dir, "Engine.app")
	cli := filepath.Join(dir, "bin", "engine")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	m := &Manifest{
		Engine: "vendor", DisplayName: "Vendor", ManifestVersion: 1,
		Platforms: map[string]Platform{runtime.GOOS + "/" + runtime.GOARCH: {
			Detect:  []string{cli},
			Install: &Install{Script: []string{"sh", "-c", "mkdir -p " + filepath.Dir(cli) + " && touch " + cli}, Creates: app},
			Runtime: Runtime{Mode: "command", Start: [][]string{{"true"}}},
		}},
	}
	ex := newTestExecutor(t, m)
	ex.detectTimeout = time.Second
	st, _ := ex.state("vendor")
	if err := writeInstallMarker(st.installDir, "vendor", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := ex.Install(context.Background(), "vendor"); err != nil {
		t.Fatal(err)
	}
	if installedByPAIR(st.installDir, app) || installedByPAIR(st.installDir, "") {
		t.Fatal("a stale claim survived an install that found the user's copy")
	}
}

// A marker written before claims recorded what they created covers nothing an
// install creates: it names no bundle, so it cannot vouch for the one there.
func TestAMarkerWithoutAnIdentityCoversNoCreatedApp(t *testing.T) {
	installDir := t.TempDir()
	if err := writeInstallMarker(installDir, "omlx", "", ""); err != nil {
		t.Fatal(err)
	}
	if installedByPAIR(installDir, "/Applications/oMLX.app") {
		t.Error("an unbound marker authorised an app it never named")
	}
	if !installedByPAIR(installDir, "") {
		t.Error("an unbound marker must still stand for an engine with no creates")
	}
}

// A claim is refused when what is at creates is no longer what the install
// put there, however soon after it was replaced.
func TestClaimIsRefusedWhenTheCreatedAppWasReplaced(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no file identity")
	}
	dir := t.TempDir()
	app := filepath.Join(dir, "Engine.app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	placed := fileIdentity(app)
	if err := os.RemoveAll(app); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	installDir := filepath.Join(dir, "install")
	if err := writeInstallMarker(installDir, "vendor", app, placed); err == nil {
		t.Fatal("a claim was recorded for a replaced app")
	}
	if installedByPAIR(installDir, app) {
		t.Fatal("a replaced app is claimed")
	}
}

// Every bundled manifest is validated on every host, so a macOS disk-image
// step must validate on Windows and Linux too.
func TestDiskImageValidatesOnEveryHost(t *testing.T) {
	p := Platform{
		Detect:  []string{"~/.omlx/bin/omlx"},
		Install: &Install{Fetch: &Fetch{URL: "https://x/y.dmg"}, DiskImage: &DiskImage{App: "oMLX.app", Into: "/Applications"}, Creates: "/Applications/oMLX.app"},
		Runtime: Runtime{Mode: "command", Start: [][]string{{"{cli}", "start"}}},
	}
	if err := p.validate("darwin/arm64"); err != nil {
		t.Fatalf("a valid disk-image platform was rejected on %s: %v", runtime.GOOS, err)
	}
	if err := p.validate("linux/amd64"); err == nil {
		t.Error("a disk-image step outside macOS was accepted")
	}
	for _, into := range []string{"Applications", "{install_dir}"} {
		bad := p
		bad.Install = &Install{Fetch: p.Install.Fetch, DiskImage: &DiskImage{App: "oMLX.app", Into: into}}
		if err := bad.validate("darwin/arm64"); err == nil {
			t.Errorf("into %q accepted", into)
		}
	}
}
