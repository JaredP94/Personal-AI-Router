// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The bundled oMLX install command, run against a disk image built here with
// the release layout (oMLX.app at its root): it copies the app, detaches the
// image, and leaves an app already in place untouched.
func TestOMLXInstallCommandCopiesTheAppFromTheDiskImage(t *testing.T) {
	if _, err := exec.LookPath("hdiutil"); err != nil {
		t.Skip("hdiutil unavailable")
	}
	m := bundledManifestSet(t)["omlx"]
	run := m.Platforms["darwin/arm64"].Install.Run
	if len(run) != 5 || run[0] != "/bin/sh" || run[4] != "{download}" {
		t.Fatalf("unexpected run argv shape: %q", run)
	}

	work := t.TempDir()
	src := filepath.Join(work, "src")
	exe := filepath.Join(src, "oMLX.app", "Contents", "MacOS", "omlx-cli")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("from-image"), 0o755); err != nil {
		t.Fatal(err)
	}
	dmg := filepath.Join(work, "oMLX.dmg")
	if out, err := exec.Command("hdiutil", "create", "-quiet", "-fs", "HFS+", "-srcfolder", src, "-volname", "oMLX", dmg).CombinedOutput(); err != nil {
		t.Skipf("hdiutil create unavailable here: %v %s", err, out)
	}

	apps := filepath.Join(work, "Applications")
	if err := os.MkdirAll(apps, 0o755); err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(apps, "oMLX.app")
	script := strings.ReplaceAll(run[2], "/Applications/oMLX.app", app)
	script = strings.ReplaceAll(script, `open -g "$app"`, `touch "$app.opened"`)
	install := func() {
		t.Helper()
		if out, err := exec.Command("/bin/sh", "-c", script, "sh", dmg).CombinedOutput(); err != nil {
			t.Fatalf("install command: %v\n%s", err, out)
		}
	}

	install()
	got, err := os.ReadFile(filepath.Join(app, "Contents", "MacOS", "omlx-cli"))
	if err != nil || string(got) != "from-image" {
		t.Fatalf("copied app = %q, %v", got, err)
	}
	if _, err := os.Stat(app + ".opened"); err != nil {
		t.Error("the app was not opened, so it never writes its CLI shim")
	}
	if out, _ := exec.Command("hdiutil", "info").Output(); strings.Contains(string(out), dmg) {
		t.Error("the disk image was left attached")
	}

	// A copy already in place is the user's: it is opened, not replaced.
	if err := os.WriteFile(filepath.Join(app, "Contents", "MacOS", "omlx-cli"), []byte("users-own"), 0o755); err != nil {
		t.Fatal(err)
	}
	install()
	if got, _ := os.ReadFile(filepath.Join(app, "Contents", "MacOS", "omlx-cli")); string(got) != "users-own" {
		t.Errorf("an app already in place was overwritten: %q", got)
	}
}
