// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCommandUninstallCoverage(t *testing.T) {
	for _, tc := range []struct {
		name, engine, cli string
		// installedByPAIR writes the install marker, as a completed PAIR
		// install does. Coverage alone is not enough: the vendor's own app
		// puts its CLI in the same place.
		installedByPAIR bool
		allowed         bool
		refusal         string
	}{
		{"Homebrew only", "omlx", "opt/homebrew/bin/omlx", true, false, "declared uninstaller does not cover the resolved CLI"},
		{"usr local only", "omlx", "usr/local/bin/omlx", true, false, "declared uninstaller does not cover the resolved CLI"},
		{"HOME oMLX", "omlx", ".omlx/bin/omlx", true, true, ""},
		{"HOME LM Studio", "lmstudio", ".lmstudio/bin/lms", true, true, ""},
		{"HOME oMLX the user installed", "omlx", ".omlx/bin/omlx", false, false, "no record of installing it"},
		{"HOME LM Studio the user installed", "lmstudio", ".lmstudio/bin/lms", false, false, "no record of installing it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := resolutionRoot(t)
			home, _ := os.UserHomeDir()
			reg := isolatedCommandRegistry(t, tc.engine, home)
			cli := filepath.Join(home, tc.cli)
			resolutionFile(t, cli)
			// Never execute the bundled vendor uninstaller (pkill/rm -rf).
			// A refused request must not even run this harmless substitute:
			// it would delete a sentinel inside the isolated .omlx directory.
			sentinel := filepath.Join(home, ".omlx", "keep")
			resolutionFile(t, sentinel)
			m, _ := reg.Get(tc.engine)
			key := runtime.GOOS + "/" + runtime.GOARCH
			p := m.Platforms[key]
			target := sentinel
			if tc.allowed {
				target = cli
			}
			// The policy under test is cli_roots coverage; ownership of what an
			// install creates is covered in omlx_install_test.go.
			if p.Install != nil {
				p.Install.Creates = ""
			}
			p.Uninstall.Run = rmCmd(target)
			stopMarker := filepath.Join(home, "stop-ran")
			p.Runtime.Stop = &StopSpec{Cmd: []string{fakeEngineBin, "touch", stopMarker}}
			m.Platforms[key] = p
			ex := NewExecutor(reg, NewReporter(nil), nil, base)
			ex.detectTimeout = 0 // never wait for a destructive-command regression
			if tc.installedByPAIR {
				st, _ := ex.state(tc.engine)
				if err := writeInstallMarker(st.installDir, tc.engine, "", ""); err != nil {
					t.Fatal(err)
				}
			}
			status, err := ex.Status(tc.engine)
			if err != nil || !status.Installed || status.Capabilities.Uninstall != tc.allowed {
				t.Errorf("status = %+v, %v; want installed and uninstall=%v", status, err, tc.allowed)
			}
			if !tc.allowed {
				st, _ := ex.state(tc.engine)
				st.running = true // refusal must precede even the stop command
			}
			err = ex.Uninstall(context.Background(), tc.engine)
			if tc.allowed {
				if err != nil {
					t.Fatalf("covered CLI uninstall: %v", err)
				}
				if fileExists(cli) {
					t.Error("covered CLI was not removed")
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.refusal) {
					t.Errorf("want refusal containing %q, got %v", tc.refusal, err)
				}
				if !fileExists(cli) {
					t.Error("external CLI was deleted")
				}
			}
			if fileExists(stopMarker) {
				t.Error("uncovered CLI was stopped before refusal")
			}
			if !fileExists(sentinel) {
				t.Error("uninstall command ran for an uncovered CLI and deleted files")
			}
		})
	}
}

// Exercise manifest policy independently of vendor names: missing declarations,
// sibling-prefix paths and traversal all fail closed; {install_dir} is supported.
func TestCommandUninstallManifestPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, roots, cli string
		allowed          bool
	}{
		{"covered", `["{install_dir}/owned"]`, "owned/bin/cli", true},
		{"missing roots", `[]`, "owned/bin/cli", false},
		{"sibling prefix", `["{install_dir}/owned"]`, "owned-other/cli", false},
		{"traversal", `["{install_dir}/owned"]`, "owned/../outside/cli", false},
		{"relative root", `["owned"]`, "owned/bin/cli", false},
		{"empty root", `[""]`, "owned/bin/cli", false},
		{"unresolvable root", `["{models_dir}"]`, "owned/bin/cli", false},
		{"multiple roots", `["{install_dir}/elsewhere","{install_dir}/owned"]`, "owned/bin/cli", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := resolutionRoot(t)
			reg := resolutionRegistry(t, `{
				"engine":"policy", "display_name":"Policy", "manifest_version":1,
				"platforms":{"`+runtime.GOOS+`/`+runtime.GOARCH+`":{
					"detect":["{install_dir}/`+tc.cli+`"],
					"runtime":{"mode":"command","start":[["{cli}","start"]]},
					"uninstall":{"cli_roots":`+tc.roots+`,"run":["unused"]}
				}}
			}`)
			cli := filepath.Join(base, "policy", tc.cli)
			if err := os.MkdirAll(filepath.Join(base, "policy", "owned"), 0o700); err != nil {
				t.Fatal(err)
			}
			resolutionFile(t, cli)
			m, _ := reg.Get("policy")
			p := m.Platforms[runtime.GOOS+"/"+runtime.GOARCH]
			p.Uninstall.Run = rmCmd(cli)
			ex := NewExecutor(reg, NewReporter(nil), nil, base)
			ex.detectTimeout = 0
			if err := writeInstallMarker(filepath.Join(base, "policy"), "policy", "", ""); err != nil {
				t.Fatal(err)
			}
			status, err := ex.Status("policy")
			if err != nil || status.Capabilities.Uninstall != tc.allowed {
				t.Errorf("status = %+v, %v; want uninstall=%v", status, err, tc.allowed)
			}
			err = ex.Uninstall(context.Background(), "policy")
			if tc.allowed {
				if err != nil || fileExists(cli) {
					t.Errorf("covered uninstall failed: %v", err)
				}
			} else if err == nil || !fileExists(cli) {
				t.Errorf("uncovered uninstall not refused safely: %v", err)
			}
		})
	}
}
