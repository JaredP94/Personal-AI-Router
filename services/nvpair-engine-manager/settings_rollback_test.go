// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	settings "nvpair-shared/enginesettings"
	"nvpair-shared/listen"
)

func TestSettingsRollbackNeverWidensPreviousBind(t *testing.T) {
	e := settingsExecutor(t, false)
	st := settingsState(t, e)
	capture := filepath.Join(t.TempDir(), "environment.json")
	st.plat.Runtime.Bind = "0.0.0.0"
	st.plat.Runtime.Env["PAIR_TEST_ENV_FILE"] = capture
	if err := e.StartWith(context.Background(), "fake", startOpts{Bind: "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	request := settingsRequest(t, e)
	request.Settings.LaunchText += " --parallel 3"
	result, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: request.Settings}, func(_ context.Context, rollback bool) error {
		if rollback {
			return nil
		}
		return fmt.Errorf("bind failed")
	})
	if err == nil || !result.Running {
		t.Fatal("previous running state not restored")
	}
	data, err := os.ReadFile(capture)
	values := map[string]string{}
	if err != nil || json.Unmarshal(data, &values) != nil {
		t.Fatal("could not inspect restored engine environment")
	}
	if values["OLLAMA_HOST"] != net.JoinHostPort("127.0.0.1", fmt.Sprint(result.EffectivePort)) {
		t.Fatal("rollback widened the previous bind address")
	}
}

func TestSettingsRollbackSurvivesApplyCancellation(t *testing.T) {
	e := settingsExecutor(t, false)
	if err := e.Start(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	request := settingsRequest(t, e)
	previous := request.Settings
	request.Settings.LaunchText += " --parallel 3"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := e.ConfigureLaunch(ctx, settings.Configure{Engine: "fake", Settings: request.Settings}, func(callbackCtx context.Context, rollback bool) error {
		if rollback {
			return callbackCtx.Err()
		}
		cancel()
		return ctx.Err()
	})
	if err == nil || !result.Running || result.LaunchText != previous.LaunchText || !strings.Contains(err.Error(), "previous settings were restored") {
		t.Fatal("cancellation abandoned compensation")
	}
}

func TestSettingsRollbackPersistenceFailureLeavesEngineStopped(t *testing.T) {
	e := settingsExecutor(t, false)
	if err := e.Start(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	request := settingsRequest(t, e)
	request.Settings.LaunchText += " --parallel 3"
	path := filepath.Join(e.overrideDir, "fake.json")
	result, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: request.Settings}, func(_ context.Context, rollback bool) error {
		if rollback {
			return nil
		}
		// Make restoration of the originally absent override fail without
		// corrupting any other file or relying on host permissions.
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "occupied"), []byte("test"), 0600); err != nil {
			t.Fatal(err)
		}
		return fmt.Errorf("bind failed")
	})
	if err == nil || result.Running || !strings.Contains(err.Error(), "restoration failed") {
		t.Fatal("failed persistence compensation restarted the engine or reported success")
	}
}

// These regressions catch returning after stop without compensating for every
// failed apply stage. All engines and files belong to the test, never the host.
func TestSettingsApplyRestoresPreviousState(t *testing.T) {
	for _, point := range []string{"persist", "rebind", "foreign-loopback", "start", "readiness", "rollback"} {
		t.Run(point, func(t *testing.T) {
			e := settingsExecutor(t, false)
			request := settingsRequest(t, e)
			if point != "persist" {
				request.Settings.LaunchText += " --parallel 2"
				if _, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: request.Settings}, func(context.Context, bool) error { return nil }); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.Start(context.Background(), "fake"); err != nil {
				t.Fatal(err)
			}
			before, err := e.LaunchSettings("fake")
			if err != nil {
				t.Fatal(err)
			}
			if point == "persist" {
				parent := t.TempDir()
				e.overrideDir = filepath.Join(parent, "missing")
				if err := os.Chmod(parent, 0500); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(parent, 0700)
			}
			path := filepath.Join(e.overrideDir, "fake.json")
			saved, err := os.ReadFile(path)
			savedExists := err == nil
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			next, err := freePort()
			if err != nil {
				t.Fatal(err)
			}
			request.Settings.ServerPort = next
			proxy, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = proxy.Close() }()
			originalProxy := proxy.Addr().(*net.TCPAddr).Port
			request.Settings.ProxyPort, err = freePort()
			if err != nil {
				t.Fatal(err)
			}
			request.Settings.LaunchText += " --parallel 3"
			switch point {
			case "start":
				request.Settings.LaunchText += " --fail-launch private-value"
			case "readiness":
				request.Settings.LaunchText = "FAKE_ADDR=127.0.0.1:0 " + request.Settings.LaunchText
				settingsState(t, e).plat.Runtime.Ready.TimeoutS = 1
			}
			result, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: request.Settings}, func(_ context.Context, rollback bool) error {
				port := request.Settings.ProxyPort
				if rollback {
					if point == "rollback" {
						return fmt.Errorf("private-value")
					}
					port = originalProxy
				}
				if point == "foreign-loopback" && !rollback {
					foreign, bindErr := net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
					if bindErr != nil {
						t.Fatal(bindErr)
					}
					defer foreign.Close()
					candidate, bindErr := listen.TCP(fmt.Sprintf(":%d", port))
					if bindErr == nil {
						candidate.Close()
					}
					return bindErr
				}
				_ = proxy.Close()
				proxy, err = net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
				if err != nil {
					t.Fatal(err)
				}
				if !rollback && (point == "rebind" || point == "rollback") {
					return fmt.Errorf("private-value")
				}
				return nil
			})
			if err == nil {
				t.Fatal("failed apply reported success")
			}
			if strings.Contains(err.Error(), "private-value") {
				t.Fatal("launch value escaped into failure")
			}
			if point == "rollback" {
				if result.Running || !strings.Contains(err.Error(), "restoration failed") {
					t.Fatalf("rollback failure not stopped/reported: running=%v err=%v", result.Running, err)
				}
			} else {
				if !strings.Contains(err.Error(), "previous settings were restored") {
					t.Errorf("missing rollback outcome: %v", err)
				}
				if !result.Running || proxy.Addr().(*net.TCPAddr).Port != originalProxy {
					t.Errorf("previous running state/proxy listener not restored: running=%v", result.Running)
				}
				conn, dialErr := net.Dial("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(before.EffectivePort)))
				if dialErr != nil {
					t.Error("previous engine listener not restored")
				} else {
					conn.Close()
				}
			}
			if result.ServerPort != before.ServerPort || result.EffectivePort != before.EffectivePort || result.LaunchText != before.LaunchText {
				t.Error("previous launch configuration/ports not restored")
			}
			after, readErr := os.ReadFile(path)
			if (readErr != nil && !os.IsNotExist(readErr)) || (readErr == nil) != savedExists || !bytes.Equal(saved, after) {
				t.Error("previous persisted configuration not restored exactly")
			}
			if errs := e.Errors(); hasErr(errs, startFailedID("fake")) || hasErr(errs, exitedID("fake")) {
				t.Error("duplicate lifecycle error")
			}
		})
	}
}
