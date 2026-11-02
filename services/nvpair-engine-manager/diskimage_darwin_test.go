// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildDiskImage makes a disk image laid out like a release: the app bundle
// at its root. An empty app name makes an image without one.
func buildDiskImage(t *testing.T, app, content string) string {
	t.Helper()
	work := t.TempDir()
	src := filepath.Join(work, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if app != "" {
		exe := filepath.Join(src, app, "Contents", "MacOS", "cli")
		if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(exe, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	} else if err := os.WriteFile(filepath.Join(src, "README"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(work, "image.dmg")
	if out, err := exec.Command(hdiutilPath, "create", "-quiet", "-fs", "HFS+", "-srcfolder", src, "-volname", "Test", image).CombinedOutput(); err != nil {
		t.Skipf("hdiutil create unavailable: %v %s", err, out)
	}
	return image
}

// leftovers reports staging copies and mounts a step left behind.
func leftovers(t *testing.T, into, image string) {
	t.Helper()
	entries, _ := os.ReadDir(into)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("staging copy left behind: %s", e.Name())
		}
	}
	if out, _ := exec.Command(hdiutilPath, "info").Output(); strings.Contains(string(out), image) {
		t.Error("the disk image was left attached")
	}
}

func TestDiskImageInstallCopiesTheApp(t *testing.T) {
	image := buildDiskImage(t, "Test.app", "from-image")
	into := t.TempDir()
	if err := installAppFromDiskImage(context.Background(), image, &DiskImage{App: "Test.app", Into: into}); err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(into, "Test.app")
	if got, err := os.ReadFile(filepath.Join(app, "Contents", "MacOS", "cli")); err != nil || string(got) != "from-image" {
		t.Fatalf("copied app = %q, %v", got, err)
	}
	if info, err := os.Stat(app); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("app mode = %v, %v; want 0755", info.Mode().Perm(), err)
	}
	leftovers(t, into, image)
}

func TestDiskImageInstallLeavesAnAppInPlace(t *testing.T) {
	image := buildDiskImage(t, "Test.app", "from-image")
	into := t.TempDir()
	exe := filepath.Join(into, "Test.app", "Contents", "MacOS", "cli")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("users-own"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installAppFromDiskImage(context.Background(), image, &DiskImage{App: "Test.app", Into: into}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "users-own" {
		t.Errorf("an app already in place was replaced: %q", got)
	}
}

// A failure after mounting detaches the image and leaves no partial app.
func TestDiskImageInstallCleansUpAfterAFailure(t *testing.T) {
	image := buildDiskImage(t, "", "")
	into := t.TempDir()
	if err := installAppFromDiskImage(context.Background(), image, &DiskImage{App: "Test.app", Into: into}); err == nil {
		t.Fatal("an image without the app installed")
	}
	if _, err := os.Lstat(filepath.Join(into, "Test.app")); !os.IsNotExist(err) {
		t.Error("an app was placed from an image that has none")
	}
	leftovers(t, into, image)
}

// Cancelling mid-copy detaches the image and leaves no partial app: the step
// owns the mount, so cleanup does not depend on the cancelled context.
func TestDiskImageInstallCleansUpAfterCancellation(t *testing.T) {
	image := buildDiskImage(t, "Test.app", strings.Repeat("x", 64<<20))
	into := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- installAppFromDiskImage(ctx, image, &DiskImage{App: "Test.app", Into: into}) }()
	// Cancel once the copy has started: a staging directory has appeared.
	for {
		entries, _ := os.ReadDir(into)
		if len(entries) > 0 {
			break
		}
		select {
		case err := <-done:
			t.Skipf("copy finished before it could be cancelled: %v", err)
		default:
		}
	}
	cancel()
	<-done
	if _, err := os.Lstat(filepath.Join(into, "Test.app")); err == nil {
		// The copy may have completed and been renamed in place before the
		// cancel landed; that is a whole app, not a partial one.
		if got, _ := os.ReadFile(filepath.Join(into, "Test.app", "Contents", "MacOS", "cli")); len(got) != 64<<20 {
			t.Error("a partial app was placed")
		}
	}
	leftovers(t, into, image)
}
