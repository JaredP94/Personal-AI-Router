// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Load the shipped platform, relocating only absolute system paths into a temp
// root. No test may inspect a developer's Homebrew install or launch a vendor CLI.
func isolatedCommandRegistry(t *testing.T, engine, root string) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := reg.LoadFS(bundledManifests, "manifests"); err != nil {
		t.Fatal(err)
	}
	m, _ := reg.Get(engine)
	plat, ok := m.PlatformFor("darwin", "arm64")
	if !ok {
		t.Fatal("missing darwin/arm64 platform")
	}
	for i, path := range plat.Detect {
		if strings.HasPrefix(path, "/") {
			plat.Detect[i] = filepath.Join(root, path)
		}
	}
	// Presence/HTTP adoption is covered separately. Keep these filesystem tests
	// independent of any real service on the engine's default port.
	plat.Runtime.Port, plat.Runtime.Ready, plat.Runtime.Health = 0, nil, nil
	m.Platforms = map[string]Platform{runtime.GOOS + "/" + runtime.GOARCH: *plat}
	reg.engines = map[string]*Manifest{engine: m}
	return reg
}

func assertCommandPresence(t *testing.T, reg *Registry, base, engine, wantPath string) {
	t.Helper()
	wantInstalled := wantPath != ""
	ex := NewExecutor(reg, NewReporter(nil), nil, base)
	if got, err := ex.Detect(engine); err != nil || got != wantInstalled {
		t.Errorf("Detect = %v, %v; want installed=%v", got, err, wantInstalled)
	}
	status, err := ex.Status(engine)
	if err != nil || status.Installed != wantInstalled {
		t.Errorf("Status = %+v, %v; want installed=%v", status, err, wantInstalled)
	}
	// Capabilities are support, not installed-ness: a missing CLI still offers
	// installation, while the authoritative installed flag remains false.
	if !status.Capabilities.Install || status.Capabilities.SetPort {
		t.Errorf("unexpected oMLX capabilities: %+v", status.Capabilities)
	}
	installed := ex.GetInstalled()
	if len(installed) != 1 || installed[0].Installed != wantInstalled {
		t.Errorf("GetInstalled = %+v; want installed=%v", installed, wantInstalled)
	}
	listed := listEngines(reg, runtime.GOOS, runtime.GOARCH)
	if len(listed) != 1 || listed[0].Installed != wantInstalled {
		t.Errorf("listEngines = %+v; want installed=%v", listed, wantInstalled)
	}
	got, err := resolveEngine(reg, engine, runtime.GOOS, runtime.GOARCH)
	if wantInstalled {
		if err != nil || got.Path != wantPath {
			t.Errorf("resolveEngine = %+v, %v; want %q", got, err, wantPath)
		}
		// An existing declared CLI must short-circuit install, not download or
		// run the real installer. Disable installation so a regression is safe.
		st, _ := ex.state(engine)
		st.plat.Install = nil
		if err := ex.Install(context.Background(), engine); err != nil {
			t.Errorf("existing CLI must skip install: %v", err)
		}
	} else if err == nil || !strings.Contains(err.Error(), "is not installed") {
		t.Errorf("resolveEngine must report not installed, got %+v, %v", got, err)
	}
}

func TestOmlxCLICandidatesAgreeAcrossConsumers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
		want  string
	}{
		{"home only", []string{".omlx/bin/omlx"}, ".omlx/bin/omlx"},
		{"homebrew only", []string{"opt/homebrew/bin/omlx"}, "opt/homebrew/bin/omlx"},
		{"usr local only", []string{"usr/local/bin/omlx"}, "usr/local/bin/omlx"},
		{"first existing wins", []string{".omlx/bin/omlx", "opt/homebrew/bin/omlx", "usr/local/bin/omlx"}, ".omlx/bin/omlx"},
		{"homebrew before usr local", []string{"opt/homebrew/bin/omlx", "usr/local/bin/omlx"}, "opt/homebrew/bin/omlx"},
		{"GUI bundle only", []string{"Applications/oMLX.app/Contents/MacOS/oMLX"}, ""},
		{"none", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := resolutionRoot(t)
			home, _ := os.UserHomeDir()
			reg := isolatedCommandRegistry(t, "omlx", home)
			for _, path := range tc.files {
				resolutionFile(t, filepath.Join(home, path))
			}
			if tc.name == "GUI bundle only" {
				// Model macOS's case-insensitive GUI lookup on case-sensitive CI.
				// A second inert file avoids requiring Windows symlink privileges.
				alias := filepath.Join(home, "Applications/oMLX.app/Contents/MacOS/omlx")
				if !fileExists(alias) {
					resolutionFile(t, alias)
				}
			}
			want := ""
			if tc.want != "" {
				want = filepath.Join(home, tc.want)
			}
			assertCommandPresence(t, reg, base, "omlx", want)
		})
	}
}

func TestCommandCLIOverrideIsAuthoritative(t *testing.T) {
	for _, exists := range []bool{true, false} {
		t.Run(map[bool]string{true: "exists", false: "missing"}[exists], func(t *testing.T) {
			base := resolutionRoot(t)
			home, _ := os.UserHomeDir()
			reg := isolatedCommandRegistry(t, "omlx", home)
			resolutionFile(t, filepath.Join(home, ".omlx/bin/omlx"))
			cli := filepath.Join(base, "omlx", "custom-cli")
			if exists {
				resolutionFile(t, cli)
			}
			dir := t.TempDir()
			data := []byte(`{"engine":"omlx","runtime":{"cli":"{install_dir}/custom-cli"}}`)
			if err := os.WriteFile(filepath.Join(dir, "omlx.json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := reg.LoadOverrideDir(dir); err != nil {
				t.Fatal(err)
			}
			// Overrides reload the bundled platform inventory. Select the same
			// shipped macOS platform on any test host and disable real probes.
			m, _ := reg.Get("omlx")
			p, _ := m.PlatformFor("darwin", "arm64")
			p.Runtime.Port, p.Runtime.Ready, p.Runtime.Health = 0, nil, nil
			m.Platforms = map[string]Platform{runtime.GOOS + "/" + runtime.GOARCH: *p}
			want := ""
			if exists {
				want = cli
			}
			assertCommandPresence(t, reg, base, "omlx", want)
		})
	}
}

func TestCommandCLISkipsDirectories(t *testing.T) {
	base := resolutionRoot(t)
	home, _ := os.UserHomeDir()
	reg := isolatedCommandRegistry(t, "omlx", home)
	directory := filepath.Join(home, ".omlx/bin/omlx")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(home, "opt/homebrew/bin/omlx")
	resolutionFile(t, cli)
	assertCommandPresence(t, reg, base, "omlx", cli)
	m, _ := reg.Get("omlx")
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := m.Platforms[key]
	p.Runtime.CLI = directory
	m.Platforms[key] = p
	assertCommandPresence(t, reg, base, "omlx", "")
}

func TestLMStudioCLIResolutionUnchanged(t *testing.T) {
	base := resolutionRoot(t)
	home, _ := os.UserHomeDir()
	reg := isolatedCommandRegistry(t, "lmstudio", home)
	cli := filepath.Join(home, ".lmstudio/bin/lms")
	resolutionFile(t, cli)
	ex := NewExecutor(reg, NewReporter(nil), nil, base)
	if installed, err := ex.Detect("lmstudio"); err != nil || !installed {
		t.Fatalf("Detect = %v, %v", installed, err)
	}
	got, err := resolveEngine(reg, "lmstudio", runtime.GOOS, runtime.GOARCH)
	if err != nil || got.Path != cli {
		t.Fatalf("LM Studio resolution = %+v, %v; want %q", got, err, cli)
	}
}

func TestCommandLifecycleAndActionsExecuteResolvedCLI(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(map[bool]string{false: "detected candidate", true: "explicit override"}[override], func(t *testing.T) {
			root := t.TempDir()
			cli := filepath.Join(root, "control-cli"+exeExt())
			copyFile(t, fakeEngineBin, cli)
			startMarker, stopMarker := filepath.Join(root, "started"), filepath.Join(root, "stopped")
			p := Platform{Detect: []string{filepath.Join(root, "missing"), cli}, Runtime: Runtime{
				Mode:  "command",
				Start: [][]string{{"{cli}", "touch", startMarker}},
				Stop:  &StopSpec{Cmd: []string{"{cli}", "touch", stopMarker}},
			}}
			if override {
				// A valid competing detection must not defeat runtime.cli.
				alternate := filepath.Join(root, "not-a-cli")
				resolutionFile(t, alternate)
				p.Detect = []string{alternate}
				p.Runtime.CLI = "{install_dir}/control-cli" + exeExt()
			}
			m := &Manifest{Engine: "fake", DisplayName: "Fake", ManifestVersion: 1,
				Platforms: map[string]Platform{runtime.GOOS + "/" + runtime.GOARCH: p},
				Actions:   map[string]Action{"identity": {Cmd: []string{"{cli}", "echo", "resolved-cli"}}}}
			ex := newTestExecutor(t, m)
			if override {
				// Expand the explicit CLI through the managed install directory.
				ex.baseDir = root
				if err := os.MkdirAll(filepath.Join(root, "fake"), 0o700); err != nil {
					t.Fatal(err)
				}
				copyFile(t, fakeEngineBin, filepath.Join(root, "fake", "control-cli"+exeExt()))
			}
			t.Cleanup(ex.StopAll)
			if err := ex.Start(context.Background(), "fake"); err != nil {
				t.Fatalf("start must execute resolved CLI: %v", err)
			}
			if !fileExists(startMarker) {
				t.Fatal("resolved start CLI did not create its marker")
			}
			out, err := ex.Action(context.Background(), "fake", "identity", json.RawMessage(`{"cli":"/untrusted"}`))
			if err != nil || string(out) != `"resolved-cli"` {
				t.Fatalf("action = %s, %v", out, err)
			}
			if err := ex.Stop("fake"); err != nil {
				t.Fatalf("stop must execute resolved CLI: %v", err)
			}
			if !fileExists(stopMarker) {
				t.Fatal("resolved stop CLI did not create its marker")
			}
		})
	}
}
