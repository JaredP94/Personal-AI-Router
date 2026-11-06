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
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	settings "nvpair-shared/enginesettings"
)

func settingsExecutor(t *testing.T, command bool) *Executor {
	t.Helper()
	m := testEngineManifest(fakeEngineForTest(t))
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := m.Platforms[key]
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	p.Runtime.Port = port
	p.Runtime.Args = []string{"serve"}
	p.Runtime.EditableLaunch = &EditableLaunch{
		FixedArgs: []string{"serve"},
		Controls: []LaunchControl{
			{Value: "{server.host}:{server.port}", Env: []string{"OLLAMA_HOST"}},
		},
	}
	if command {
		p.Runtime.Mode = "command"
		p.Runtime.CLI = fakeEngineForTest(t)
		p.Runtime.Start = [][]string{{"{cli}", "server", "start", "--port", "{port}", "--bind", "{host}"}}
		p.Runtime.EditableLaunch = &EditableLaunch{
			FixedArgs: []string{"server", "start"},
			Controls: []LaunchControl{
				{Value: "{server.port}", Flags: []string{"--port", "-p"}, Env: []string{"LMS_SERVER_PORT"}},
				{Value: "{server.host}", Flags: []string{"--bind", "--host"}, Env: []string{"LMS_SERVER_HOST"}},
			},
		}
	}
	m.Platforms[key] = p
	e := newTestExecutor(t, m)
	e.overrideDir = t.TempDir()
	if _, err := e.Detect("fake"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Stop("fake") })
	return e
}

func settingsRequest(t *testing.T, e *Executor) settings.Request {
	t.Helper()
	s, err := e.LaunchSettings("fake")
	if err != nil {
		t.Fatal(err)
	}
	return settings.Request{Engine: "fake", Settings: settings.Config{ServerPort: s.ServerPort, ProxyPort: 54301, LaunchText: s.LaunchText}}
}

// eachEngineMode runs test against both Runtime.Mode lifecycles: "process",
// where this service spawns the engine and its managed controls bind to
// environment variables, and "command", where a control CLI brings the engine
// up and the same controls bind to flags. The launch settings path must behave
// identically either way, so every mode-independent test here runs twice.
func eachEngineMode(t *testing.T, test func(*testing.T, *Executor)) {
	t.Helper()
	t.Run("process mode", func(t *testing.T) { test(t, settingsExecutor(t, false)) })
	t.Run("command mode", func(t *testing.T) { test(t, settingsExecutor(t, true)) })
}

func previewSettings(t *testing.T, e *Executor, request settings.Request) settings.Preview {
	t.Helper()
	preview, err := e.PreviewLaunch(request)
	if err != nil {
		t.Fatalf("PreviewLaunch: %v", err)
	}
	return preview
}

func settingsState(t *testing.T, e *Executor) *engineState {
	t.Helper()
	state, err := e.state("fake")
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func assertNoSettingsOverride(t *testing.T, e *Executor) {
	t.Helper()
	entries, err := os.ReadDir(e.overrideDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("rejected operation wrote configuration")
	}
}

func TestSettingsPreviewPreservesLiteralArguments(t *testing.T) {
	eachEngineMode(t, func(t *testing.T, e *Executor) {
		request := settingsRequest(t, e)
		request.Settings.LaunchText += ` --parallel 3 "two words" "" "{port}" "$HOME"`
		preview := previewSettings(t, e, request)
		if len(preview.Errors) != 0 || preview.Conflict != nil {
			t.Fatalf("valid preview: %+v", preview)
		}
		want := []string{"--parallel", "3", "two words", "", "{port}", "$HOME"}
		if !reflect.DeepEqual(preview.Args, want) {
			t.Fatalf("literal args: %#v, want %#v", preview.Args, want)
		}
		assertNoSettingsOverride(t, e)
	})
}

func TestSettingsServerPortChangesOnlyThroughDedicatedField(t *testing.T) {
	eachEngineMode(t, func(t *testing.T, e *Executor) {
		request := settingsRequest(t, e)
		request.Settings.ServerPort++
		preview := previewSettings(t, e, request)
		if len(preview.Errors) != 0 || preview.Conflict != nil || preview.Settings.ServerPort != request.Settings.ServerPort {
			t.Fatal("dedicated port edit rejected")
		}
		assertNoSettingsOverride(t, e)
	})
}

func TestSettingsPreviewChecksOnlyDeclaredControls(t *testing.T) {
	eachEngineMode(t, func(t *testing.T, e *Executor) {
		for _, suffix := range []string{" | other", " && other"} {
			p := settingsRequest(t, e)
			p.Settings.LaunchText += suffix
			if result := previewSettings(t, e, p); result.Errors["launchText"] == "" {
				t.Fatalf("accepted shell syntax: %q", suffix)
			}
		}
		policy := settingsState(t, e).plat.Runtime.EditableLaunch
		if policy.Controls[0].Value == "{server.port}" {
			for _, suffix := range []string{" --bind 0.0.0.0", " --port 0", " -- --bind 0.0.0.0"} {
				p := settingsRequest(t, e)
				p.Settings.LaunchText += suffix
				if result := previewSettings(t, e, p); result.Errors["launchText"] == "" {
					t.Fatalf("accepted invalid managed control: %q", suffix)
				}
			}
		} else {
			p := settingsRequest(t, e)
			p.Settings.LaunchText = "OLLAMA_HOST=0.0.0.0:12345"
			if result := previewSettings(t, e, p); result.Errors["launchText"] == "" {
				t.Fatal("accepted invalid managed host")
			}
		}
		assertNoSettingsOverride(t, e)
	})
}

func TestSettingsDoesNotGuessNetworkOptionSemantics(t *testing.T) {
	eachEngineMode(t, func(t *testing.T, e *Executor) {
		p := settingsRequest(t, e)
		want := []string{"--bind-address", "0.0.0.0", "--set", "host=0.0.0.0", "--set=port=22", "--set=--bind=0.0.0.0", "--hostname=anything", "--listen=anything"}
		text, err := formatLaunchText(want)
		if err != nil {
			t.Fatal(err)
		}
		p.Settings.LaunchText += " " + text
		result := previewSettings(t, e, p)
		if len(result.Errors) != 0 || !reflect.DeepEqual(result.Args, want) {
			t.Fatalf("opaque options changed or rejected: %+v", result)
		}
	})
}

func TestSettingsPreviewAcceptsUnrelatedOptions(t *testing.T) {
	eachEngineMode(t, func(t *testing.T, e *Executor) {
		request := settingsRequest(t, e)
		want := []string{"--transport", "grpc", "-v", "--max-tokens", "not-a-number",
			"--future-engine-option", "unknown-value", "-batch", "-ctx-size", "4096",
			"--config", "custom.json", "--config-file=custom.json", "--api-key", "test-value",
			"--token=test-value", "--password=test-value", "--secret=test-value",
			"--set=transport=grpc", "--label=host", "--set=api-key=test-value", "--set=token=test-value"}
		text, err := formatLaunchText(want)
		if err != nil {
			t.Fatal(err)
		}
		request.Settings.LaunchText += " " + text
		preview := previewSettings(t, e, request)
		if len(preview.Errors) != 0 || preview.Conflict != nil {
			t.Fatalf("safe options rejected: %+v", preview)
		}
		if !reflect.DeepEqual(preview.Args, want) {
			t.Fatalf("args = %v, want %v", preview.Args, want)
		}
	})
}

func TestSettingsArgumentsDoNotIncludeExecutableOrSubcommand(t *testing.T) {
	eachEngineMode(t, func(t *testing.T, e *Executor) {
		p := settingsRequest(t, e)
		if strings.Contains(p.Settings.LaunchText, fakeEngineForTest(t)) || strings.Contains(p.Settings.LaunchText, "server start") || strings.Contains(p.Settings.LaunchText, "serve") {
			t.Fatalf("manifest command leaked into editor: %q", p.Settings.LaunchText)
		}
		p.Settings.LaunchText = "different serve"
		result := previewSettings(t, e, p)
		if len(result.Errors) != 0 {
			t.Fatalf("positional arguments rejected: %+v", result)
		}
		st := settingsState(t, e)
		rt := st.plat.Runtime
		rt.LaunchArgs = &result.Args
		rt.LaunchEnv = &result.Env
		launch, err := launchForState(st, p.Settings.ServerPort)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := applyLiteralLaunch(rt, launch, map[string]string{"host": "127.0.0.1", "port": fmt.Sprint(p.Settings.ServerPort)})
		if err != nil || actual.Bin != fakeEngineForTest(t) || !slices.Equal(actual.Args[:len(rt.EditableLaunch.FixedArgs)], rt.EditableLaunch.FixedArgs) {
			t.Fatalf("arguments changed the executable/subcommand: %+v %v", actual, err)
		}
	})
}

func TestSettingsEmptyArgumentsRestoreManagedDefaults(t *testing.T) {
	eachEngineMode(t, func(t *testing.T, e *Executor) {
		p := settingsRequest(t, e)
		p.Settings.LaunchText = ""
		result := previewSettings(t, e, p)
		if len(result.Errors) != 0 || len(result.Args) != 0 || len(result.Env) != 0 || result.Settings.ServerPort != p.Settings.ServerPort {
			t.Fatalf("empty arguments did not restore defaults: %+v", result)
		}
	})
}

func readSettingsOverride(t *testing.T, e *Executor) Runtime {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(e.overrideDir, "fake.json"))
	if err != nil {
		t.Fatal(err)
	}
	var override struct {
		Platforms map[string]struct {
			Runtime Runtime `json:"runtime"`
		} `json:"platforms"`
	}
	if err := json.Unmarshal(data, &override); err != nil {
		t.Fatalf("decode override: %v", err)
	}
	platform, ok := override.Platforms[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		t.Fatal("host platform missing from override")
	}
	return platform.Runtime
}

// lifecycle is what applying settings should do to the engine process.
type lifecycle int

const (
	staysStopped lifecycle = iota
	preservesProcess
	restartsProcess
)

func TestSettingsConfigureLifecycle(t *testing.T) {
	test := func(name string, want lifecycle, edit func(*testing.T, *settings.Request)) {
		t.Run(name, func(t *testing.T) {
			e := settingsExecutor(t, false)
			st := settingsState(t, e)
			startLog := filepath.Join(t.TempDir(), "starts.jsonl")
			st.plat.Runtime.Env["FAKE_START_LOG"] = startLog
			if want != staysStopped {
				if err := e.Start(context.Background(), "fake"); err != nil {
					t.Fatal(err)
				}
			}
			pid := func() int {
				st.mu.Lock()
				defer st.mu.Unlock()
				if st.proc == nil {
					return 0
				}
				return st.proc.cmd.Process.Pid
			}
			before := pid()
			request := settingsRequest(t, e)
			edit(t, &request)
			preview := previewSettings(t, e, request)
			if len(preview.Errors) != 0 || preview.Conflict != nil {
				t.Fatalf("preview: %+v", preview)
			}
			rebinds := 0
			result, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: preview.Settings}, func(context.Context, bool) error { rebinds++; return nil })
			if err != nil {
				t.Fatal(err)
			}
			got := staysStopped
			if result.Running {
				got = preservesProcess
			}
			if pid() != before {
				got = restartsProcess
			}
			if got != want || rebinds != 1 {
				t.Fatalf("result=%+v pid=%d previous=%d rebinds=%d", result, pid(), before, rebinds)
			}
			if result.ServerPort != preview.Settings.ServerPort {
				t.Fatalf("port=%d, want %d", result.ServerPort, preview.Settings.ServerPort)
			}
			override := readSettingsOverride(t, e)
			if override.LaunchArgs == nil || !reflect.DeepEqual(*override.LaunchArgs, preview.Args) {
				t.Fatalf("persisted arguments = %v, want %v", override.LaunchArgs, preview.Args)
			}
			if want != staysStopped {
				data, err := os.ReadFile(startLog)
				if err != nil {
					t.Fatal(err)
				}
				launches := 1
				if want == restartsProcess {
					launches++
				}
				if count := bytes.Count(data, []byte("\n")); count != launches {
					t.Fatalf("launch count=%d, want %d", count, launches)
				}
			} else if _, err := os.Stat(startLog); !os.IsNotExist(err) {
				t.Fatalf("stopped engine launched: %v", err)
			}
		})
	}
	test("no-op preserves process", preservesProcess, func(*testing.T, *settings.Request) {})
	test("proxy port preserves process", preservesProcess, func(t *testing.T, request *settings.Request) {
		request.Settings.ProxyPort++
	})
	test("literal arguments restart process", restartsProcess, func(t *testing.T, request *settings.Request) {
		request.Settings.LaunchText += ` --parallel 4 "{port}"`
	})
	test("server port restarts process", restartsProcess, func(t *testing.T, request *settings.Request) {
		port, err := freePort()
		if err != nil {
			t.Fatal(err)
		}
		request.Settings.ServerPort = port
		request.Resolution = "server"
	})
	test("stopped engine stays stopped", staysStopped, func(t *testing.T, request *settings.Request) {
		request.Settings.LaunchText += " --another-option"
	})
}

func TestSettingsNoOpApplyKeepsEngineLogCapture(t *testing.T) {
	e := settingsExecutor(t, false)
	st := settingsState(t, e)
	if err := e.Start(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	if len(st.logs.snapshot()) == 0 {
		t.Fatal("no engine output captured before Apply")
	}
	request := settingsRequest(t, e)
	preview, err := e.PreviewLaunch(request)
	if err != nil || len(preview.Errors) > 0 {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	if _, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: preview.Settings}, func(context.Context, bool) error { return nil }); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	persisted := st.plat.Runtime.LaunchEnv
	st.mu.Unlock()
	if persisted == nil || len(*persisted) != 0 {
		t.Fatalf("expected a no-op Apply to leave an empty launch_env, got %v", persisted)
	}
	if err := e.Stop("fake"); err != nil {
		t.Fatal(err)
	}
	before := len(st.logs.snapshot())
	if err := e.Start(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	if len(st.logs.snapshot()) == before {
		t.Fatal("engine log capture stopped after a no-op Apply persisted an empty launch_env")
	}
}

func TestSettingsEarlyLaunchFailureRestoresPreviousOptions(t *testing.T) {
	e := settingsExecutor(t, false)
	if err := e.Start(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	p := settingsRequest(t, e)
	p.Settings.LaunchText += " --fail-launch private-value"
	started := time.Now()
	result, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: p.Settings}, func(context.Context, bool) error { return nil })
	if err == nil || !result.Running || !strings.Contains(err.Error(), "previous settings were restored") {
		t.Fatalf("early failure misreported: %+v %v", result, err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("early exit waited through readiness timeout")
	}
	if strings.Contains(result.LaunchText, "--fail-launch") {
		t.Fatal("failed arguments retained after rollback")
	}
	if strings.Contains(err.Error(), "private-value") {
		t.Fatal("vendor echo escaped into error")
	}
	if errors := e.Errors(); hasErr(errors, startFailedID("fake")) || hasErr(errors, exitedID("fake")) {
		t.Fatalf("settings failure was also reported through the global dialog: %+v", errors)
	}
}

func TestEarlyExitHasOneStartError(t *testing.T) {
	e := settingsExecutor(t, false)
	p := settingsRequest(t, e)
	p.Settings.LaunchText += " --fail-launch"
	if _, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: p.Settings}, func(context.Context, bool) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(context.Background(), "fake"); err == nil {
		t.Fatal("expected failed start")
	}
	if errors := e.Errors(); len(errors) != 1 || !hasErr(errors, startFailedID("fake")) || hasErr(errors, exitedID("fake")) {
		t.Fatalf("expected one start failure, got %+v", errors)
	}
}

func TestSettingsPassesUnknownEnvironmentNamesAndValues(t *testing.T) {
	eachEngineMode(t, func(t *testing.T, e *Executor) {
		request := settingsRequest(t, e)
		want := []string{"FUTURE_ENGINE_SETTING=unknown-value", "OLLAMA_CONTEXT_LENGTH=not-a-number",
			"OLLAMA_HOSTNAME=example.test", "PATH=/custom", "LD_LIBRARY_PATH=/custom/lib"}
		text, err := formatLaunchText(want)
		if err != nil {
			t.Fatal(err)
		}
		request.Settings.LaunchText = text + " " + request.Settings.LaunchText
		preview := previewSettings(t, e, request)
		if len(preview.Errors) != 0 || preview.Conflict != nil {
			t.Fatalf("opaque environment rejected: %+v", preview)
		}
		if !reflect.DeepEqual(preview.Env, want) {
			t.Fatalf("environment = %v, want %v", preview.Env, want)
		}
		assertNoSettingsOverride(t, e)
	})
}

func manifestSettingsRequest(t *testing.T, platform string) (*Executor, settings.Request, []string) {
	t.Helper()
	reg := loadWithOverrides(t, t.TempDir())
	manifest, ok := reg.Get("ollama")
	if !ok {
		t.Fatal("Ollama manifest missing")
	}
	config, ok := manifest.Platforms[platform]
	if !ok {
		t.Fatalf("platform %q missing", platform)
	}
	e := settingsExecutor(t, false)
	settingsState(t, e).plat.Runtime = config.Runtime
	request := settingsRequest(t, e)
	tokens, err := parseLaunchText(request.Settings.LaunchText)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) == 0 || !strings.HasPrefix(tokens[0], "LD_LIBRARY_PATH=") {
		t.Fatalf("missing manifest library path: %v", tokens)
	}
	return e, request, tokens
}

func TestSettingsRejectsDuplicateManifestEnvironment(t *testing.T) {
	e, request, tokens := manifestSettingsRequest(t, "linux/arm64")
	request.Settings.LaunchText = tokens[0] + " " + request.Settings.LaunchText
	if preview := previewSettings(t, e, request); preview.Errors["launchText"] == "" {
		t.Fatal("duplicate default environment assignment accepted")
	}
	assertNoSettingsOverride(t, e)
}

func TestSettingsManifestEnvironmentCanBeOverridden(t *testing.T) {
	test := func(name, platform, replacement string) {
		t.Run(name, func(t *testing.T) {
			e, request, tokens := manifestSettingsRequest(t, platform)
			tokens[0] = "LD_LIBRARY_PATH=" + replacement
			var err error
			request.Settings.LaunchText, err = formatLaunchText(tokens)
			if err != nil {
				t.Fatal(err)
			}
			if preview := previewSettings(t, e, request); len(preview.Errors) != 0 {
				t.Fatalf("rejected replacement library path %q: %+v", replacement, preview)
			}
			if _, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: request.Settings}, func(context.Context, bool) error { return nil }); err != nil {
				t.Fatal(err)
			}
			override := readSettingsOverride(t, e)
			env, err := literalEnvironment(*override.LaunchEnv)
			if err != nil || env["LD_LIBRARY_PATH"] != replacement {
				t.Fatalf("saved environment = %v, error %v", env, err)
			}
		})
	}
	test("amd64 custom directory", "linux/amd64", "/custom/libraries")
	test("amd64 empty directory", "linux/amd64", "")
	test("amd64 literal template", "linux/amd64", "{install_dir}/other")
	test("arm64 custom directory", "linux/arm64", "/custom/libraries")
	test("arm64 empty directory", "linux/arm64", "")
	test("arm64 literal template", "linux/arm64", "{install_dir}/other")
}

func TestSettingsSafeEditPreservesManifestEnvironment(t *testing.T) {
	test := func(name, platform string, omitFixed bool) {
		t.Run(name, func(t *testing.T) {
			e, request, tokens := manifestSettingsRequest(t, platform)
			if omitFixed {
				var err error
				request.Settings.LaunchText, err = formatLaunchText(tokens[1:])
				if err != nil {
					t.Fatal(err)
				}
			}
			request.Settings.LaunchText = "OLLAMA_NUM_PARALLEL=3 " + request.Settings.LaunchText
			preview := previewSettings(t, e, request)
			if len(preview.Errors) != 0 {
				t.Fatalf("safe edit rejected: %+v", preview)
			}
			actual, err := parseLaunchText(preview.Settings.LaunchText)
			if err != nil {
				t.Fatal(err)
			}
			if len(actual) == 0 || actual[0] != tokens[0] {
				t.Fatalf("safe edit lost fixed library path: %v", actual)
			}
			if _, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: request.Settings}, func(context.Context, bool) error { return nil }); err != nil {
				t.Fatal(err)
			}
			state := settingsState(t, e)
			state.installDir = "/updated-install"
			launch, err := launchForState(state, request.Settings.ServerPort)
			if err != nil || launch.Env["LD_LIBRARY_PATH"] != "/updated-install/lib/ollama" {
				t.Fatalf("default environment stopped following the manifest: %v, %v", launch.Env, err)
			}
		})
	}
	test("amd64 retains fixed assignment", "linux/amd64", false)
	test("amd64 restores omitted assignment", "linux/amd64", true)
	test("arm64 retains fixed assignment", "linux/arm64", false)
	test("arm64 restores omitted assignment", "linux/arm64", true)
}

func TestSettingsBudgetCoversBundledReadiness(t *testing.T) {
	reg := loadWithOverrides(t, t.TempDir())
	for _, name := range reg.Names() {
		manifest, ok := reg.Get(name)
		if !ok {
			t.Fatalf("manifest %q missing", name)
		}
		for platform, config := range manifest.Platforms {
			rt := config.Runtime
			if rt.EditableLaunch == nil || rt.Ready == nil {
				continue
			}
			required := time.Duration(rt.Ready.TimeoutS) * time.Second
			if rt.Stop != nil {
				required += time.Duration(rt.Stop.GraceS) * time.Second
			}
			if settings.ConfigureBudget <= required {
				t.Errorf("%s/%s: configure budget %s cuts off stop/readiness allowance %s", name, platform, settings.ConfigureBudget, required)
			}
		}
	}
	if settings.OperationBudget <= settings.ConfigureBudget || settings.CallBudget <= settings.OperationBudget || settings.RelayBudget <= settings.CallBudget {
		t.Fatal("settings callers must leave headroom above the operation they await")
	}
}

func TestSavedLaunchOverridesManifestEnvironmentLiterally(t *testing.T) {
	rt := Runtime{
		EditableLaunch: &EditableLaunch{FixedArgs: []string{"serve"}},
		Env:            map[string]string{"LD_LIBRARY_PATH": "{install_dir}/lib"},
	}
	args := []string{}
	rt.LaunchArgs = &args
	for _, value := range []string{"/custom", "", "{install_dir}/other"} {
		env := []string{"LD_LIBRARY_PATH=" + value}
		rt.LaunchEnv = &env
		launch, err := resolveProcessLaunch(rt, "engine", map[string]string{"install_dir": "/default", "host": "127.0.0.1", "port": "12345"})
		if err != nil || launch.Env["LD_LIBRARY_PATH"] != value {
			t.Errorf("saved launch environment = %v, error %v; want literal %q", launch.Env, err, value)
		}
	}
}

func TestSettingsSwapsLiveServerAndProxyPorts(t *testing.T) {
	e := settingsExecutor(t, false)
	if err := e.Start(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	p := settingsRequest(t, e)
	oldServer := p.Settings.ServerPort
	p.Settings.ServerPort = proxy.Addr().(*net.TCPAddr).Port
	p.Settings.ProxyPort = oldServer
	p.Resolution = "server"
	preview, err := e.PreviewLaunch(p)
	if err != nil || len(preview.Errors) != 0 || preview.Conflict != nil {
		t.Fatalf("swap preview: %+v %v", preview, err)
	}
	rebinds := 0
	result, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: preview.Settings}, func(context.Context, bool) error {
		rebinds++
		next, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", oldServer))
		if err != nil {
			return err
		}
		_ = proxy.Close()
		proxy = next
		return nil
	})
	if err != nil || !result.Running || result.EffectivePort != p.Settings.ServerPort || rebinds != 1 {
		t.Fatalf("live swap: %+v rebinds=%d err=%v", result, rebinds, err)
	}
}

func TestSettingsCommandLaunchPreservesLiteralsAndCleansFailedStart(t *testing.T) {
	e := settingsExecutor(t, true)
	st := settingsState(t, e)
	capture := filepath.Join(t.TempDir(), "argv.json")
	stopped := filepath.Join(t.TempDir(), "stopped")
	rt := &st.plat.Runtime
	rt.Start = [][]string{{fakeEngineForTest(t), "captureargs", capture, "--port", "{port}", "--bind", "{host}"}}
	rt.EditableLaunch.FixedArgs = []string{"captureargs", capture}
	rt.Ready = nil
	rt.Health = nil
	rt.Stop = &StopSpec{Cmd: []string{fakeEngineForTest(t), "touch", stopped}}
	p := settingsRequest(t, e)
	p.Settings.LaunchText += ` "{port}" "$HOME" "two words" ""`
	if _, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: p.Settings}, func(context.Context, bool) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(capture)
	var args []string
	if err != nil || json.Unmarshal(data, &args) != nil || len(args) < 4 || !reflect.DeepEqual(args[len(args)-4:], []string{"{port}", "$HOME", "two words", ""}) {
		t.Fatalf("command mode expanded literal options: %s %v", data, err)
	}
	if err := e.Stop("fake"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(stopped); err != nil {
		t.Fatal(err)
	}
	p = settingsRequest(t, e)
	p.Settings.LaunchText += " --fail-launch private-value"
	if _, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: p.Settings}, func(context.Context, bool) error { return nil }); err != nil {
		t.Fatal(err)
	}
	err = e.Start(context.Background(), "fake")
	if err == nil || strings.Contains(err.Error(), "private-value") {
		t.Fatalf("failed command output was not classified safely: %v", err)
	}
	if _, err := os.Stat(stopped); err != nil {
		t.Fatal("failed first command did not run official cleanup")
	}
}

func TestSettingsEnvironmentReachesEngineAndCanBeEditedAndRemoved(t *testing.T) {
	for _, command := range []bool{false, true} {
		t.Run(fmt.Sprint(command), func(t *testing.T) {
			e := settingsExecutor(t, command)
			st := settingsState(t, e)
			if command {
				capture := filepath.Join(t.TempDir(), "args.json")
				rt := &st.plat.Runtime
				rt.Start = [][]string{{fakeEngineForTest(t), "captureargs", capture, "--port", "{port}", "--bind", "{host}"}}
				rt.EditableLaunch.FixedArgs = []string{"captureargs", capture}
				rt.Ready, rt.Health = nil, nil
				rt.Stop = &StopSpec{Cmd: []string{fakeEngineForTest(t), "touch", filepath.Join(t.TempDir(), "stopped")}}
			}
			captureEnv := filepath.Join(t.TempDir(), "env.json")
			prefix, err := formatLaunchText([]string{"PAIR_TEST_ENV_FILE=" + captureEnv, "PAIR_TEST_CUSTOM=first-value", `PAIR_TEST_LITERAL=$HOME {port} C:\new\tools`, "PAIR_TEST_EMPTY="})
			if err != nil {
				t.Fatal(err)
			}
			p := settingsRequest(t, e)
			p.Settings.LaunchText = prefix + " " + p.Settings.LaunchText
			configure := func() settings.LaunchState {
				result, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: p.Settings}, func(context.Context, bool) error { return nil })
				if err != nil {
					t.Fatal(err)
				}
				return result
			}
			if configure().Running {
				t.Fatal("environment edit started stopped engine")
			}
			if err := e.Start(context.Background(), "fake"); err != nil {
				t.Fatal(err)
			}
			readEnv := func() map[string]string {
				data, err := os.ReadFile(captureEnv)
				if err != nil {
					t.Fatal(err)
				}
				var env map[string]string
				if err := json.Unmarshal(data, &env); err != nil {
					t.Fatal(err)
				}
				return env
			}
			if env := readEnv(); env["PAIR_TEST_CUSTOM"] != "first-value" || env["PAIR_TEST_LITERAL"] != `$HOME {port} C:\new\tools` || env["PAIR_TEST_EMPTY"] != "" {
				t.Fatalf("environment not passed literally: %#v", env)
			}
			p = settingsRequest(t, e)
			p.Settings.LaunchText = strings.Replace(p.Settings.LaunchText, `PAIR_TEST_CUSTOM="first-value"`, `PAIR_TEST_CUSTOM="second-value"`, 1)
			if !configure().Running || readEnv()["PAIR_TEST_CUSTOM"] != "second-value" {
				t.Fatal("environment-only edit did not restart with new value")
			}
			p = settingsRequest(t, e)
			p.Settings.LaunchText = strings.Replace(p.Settings.LaunchText, `PAIR_TEST_CUSTOM="second-value" `, "", 1)
			configure()
			if readEnv()["PAIR_TEST_CUSTOM"] != "" {
				t.Fatal("removed environment variable survived")
			}
			override := readSettingsOverride(t, e)
			if override.LaunchEnv == nil {
				t.Fatal("launch environment missing from override")
			}
			env, err := literalEnvironment(*override.LaunchEnv)
			if err != nil {
				t.Fatal(err)
			}
			if _, exists := env["PAIR_TEST_CUSTOM"]; exists {
				t.Fatal("removed environment variable remains in override")
			}
			if env["PAIR_TEST_ENV_FILE"] != captureEnv || env["PAIR_TEST_LITERAL"] != `$HOME {port} C:\new\tools` {
				t.Fatalf("unrelated environment values lost: %v", env)
			}
		})
	}
}

func TestSettingsEnvironmentValidation(t *testing.T) {
	e := settingsExecutor(t, false)
	for _, prefix := range []string{"9BAD=value", "BAD-NAME=value", "PAIR_TEST=x PAIR_TEST=y"} {
		p := settingsRequest(t, e)
		p.Settings.LaunchText = prefix + " " + p.Settings.LaunchText
		result, err := e.PreviewLaunch(p)
		if err != nil || len(result.Errors) == 0 {
			t.Fatalf("invalid assignment accepted: %+v %v", result, err)
		}
	}
}

func TestSettingsFailedStopLeavesSavedConfigurationIntact(t *testing.T) {
	e := settingsExecutor(t, true)
	st := settingsState(t, e)
	capture := filepath.Join(t.TempDir(), "argv.json")
	rt := &st.plat.Runtime
	rt.Start = [][]string{{fakeEngineForTest(t), "captureargs", capture, "--port", "{port}", "--bind", "{host}"}}
	rt.EditableLaunch.FixedArgs = []string{"captureargs", capture}
	rt.Ready, rt.Health = nil, nil
	rt.Stop = &StopSpec{Cmd: []string{fakeEngineForTest(t), "failmark", filepath.Join(t.TempDir(), "stop-attempts")}}
	if err := e.Start(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	override := filepath.Join(e.overrideDir, "fake.json")
	before, err := os.ReadFile(override)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	request := settingsRequest(t, e)
	request.Settings.LaunchText += " --parallel 7"
	result, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: request.Settings}, func(context.Context, bool) error {
		t.Error("rebound an engine that could not be stopped")
		return nil
	})
	if err == nil {
		t.Fatalf("failed stop reported success: %+v", result)
	}
	after, readErr := os.ReadFile(override)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("stop failure still rewrote saved configuration: %q -> %q", before, after)
	}
	live := e.launchStateLocked("fake", st)
	if strings.Contains(live.LaunchText, "--parallel 7") {
		t.Fatalf("in-memory launch adopted the rejected change: %q", live.LaunchText)
	}
}

func TestSettingsRejectBeforeMutationAndRestoreAcceptedFailure(t *testing.T) {
	e := settingsExecutor(t, false)
	request := settingsRequest(t, e)
	st := settingsState(t, e)
	st.mu.Lock()
	st.running = true
	st.adopted = true
	st.mu.Unlock()
	request.Settings.LaunchText += " --new"
	if _, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: request.Settings}, func(context.Context, bool) error { t.Fatal("rebound adopted engine"); return nil }); err == nil {
		t.Fatal("adopted process accepted")
	}
	assertNoSettingsOverride(t, e)
	st.mu.Lock()
	st.running = false
	st.adopted = false
	st.mu.Unlock()
	result, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: request.Settings}, func(_ context.Context, rollback bool) error {
		if rollback {
			return nil
		}
		return fmt.Errorf("bind failed")
	})
	if err == nil || result.Running || strings.Contains(result.LaunchText, "--new") || !strings.Contains(err.Error(), "previous settings were restored") {
		t.Fatalf("previous stopped configuration not restored: %+v %v", result, err)
	}
	assertNoSettingsOverride(t, e)
}

func TestSettingsSupportsEngineWithoutStartupSubcommand(t *testing.T) {
	e := settingsExecutor(t, false)
	st := settingsState(t, e)
	st.plat.Runtime.Args = nil
	st.plat.Runtime.EditableLaunch.FixedArgs = nil
	if err := st.plat.validate(runtime.GOOS + "/" + runtime.GOARCH); err != nil {
		t.Fatal(err)
	}
	p := settingsRequest(t, e)
	p.Settings.LaunchText += " --future-option=opaque"
	preview := previewSettings(t, e, p)
	if len(preview.Errors) != 0 || !reflect.DeepEqual(preview.Args, []string{"--future-option=opaque"}) {
		t.Fatalf("engine without subcommand rejected: %+v", preview)
	}
}

func TestSettingsOmlxLaunchStateAndRestrictions(t *testing.T) {
	m := &Manifest{
		Engine:          "omlx",
		DisplayName:     "oMLX",
		ManifestVersion: 1,
		Platforms: map[string]Platform{
			runtime.GOOS + "/" + runtime.GOARCH: {
				Runtime: Runtime{
					Bin:  fakeEngineForTest(t),
					Port: 1235,
					Args: []string{"serve"},
				},
			},
		},
	}
	e := newTestExecutor(t, m)
	e.overrideDir = t.TempDir()

	// LaunchState: editable true, ServerPort 1235, LaunchText empty.
	state, err := e.LaunchSettings("omlx")
	if err != nil {
		t.Fatalf("LaunchSettings(omlx): %v", err)
	}
	if !state.Editable {
		t.Fatalf("expected omlx Editable=true, got %v (%s)", state.Editable, state.Reason)
	}
	if state.ServerPort != 1235 {
		t.Fatalf("expected omlx ServerPort=1235, got %d", state.ServerPort)
	}
	if state.LaunchText != "" {
		t.Fatalf("expected empty LaunchText for omlx, got %q", state.LaunchText)
	}

	// PreviewLaunch: rejects changed server port.
	previewPort, err := e.PreviewLaunch(settings.Request{
		Engine:   "omlx",
		Settings: settings.Config{ServerPort: 9999, ProxyPort: 54321, LaunchText: ""},
	})
	if err != nil {
		t.Fatalf("PreviewLaunch: %v", err)
	}
	if previewPort.Errors["serverPort"] == "" {
		t.Fatal("expected error on changed omlx ServerPort")
	}

	// PreviewLaunch: rejects non-empty launch text.
	previewText, err := e.PreviewLaunch(settings.Request{
		Engine:   "omlx",
		Settings: settings.Config{ServerPort: 1235, ProxyPort: 54321, LaunchText: "--custom-flag"},
	})
	if err != nil {
		t.Fatalf("PreviewLaunch: %v", err)
	}
	if previewText.Errors["launchText"] == "" {
		t.Fatal("expected error on non-empty omlx LaunchText")
	}

	// PreviewLaunch: accepts proxyPort change.
	previewValid, err := e.PreviewLaunch(settings.Request{
		Engine:   "omlx",
		Settings: settings.Config{ServerPort: 1235, ProxyPort: 54321, LaunchText: ""},
	})
	if err != nil {
		t.Fatalf("PreviewLaunch: %v", err)
	}
	if len(previewValid.Errors) != 0 {
		t.Fatalf("unexpected errors on valid proxyPort change: %v", previewValid.Errors)
	}
	if previewValid.Restart {
		t.Fatal("proxy-only change should not request restart")
	}

	// ConfigureLaunch: rejects changed serverPort.
	_, err = e.ConfigureLaunch(context.Background(), settings.Configure{
		Engine:   "omlx",
		Settings: settings.Config{ServerPort: 9999, ProxyPort: 54321, LaunchText: ""},
	}, func(context.Context, bool) error { return nil })
	if err == nil {
		t.Fatal("expected ConfigureLaunch to reject changed serverPort")
	}

	// ConfigureLaunch: rejects non-empty launch text.
	_, err = e.ConfigureLaunch(context.Background(), settings.Configure{
		Engine:   "omlx",
		Settings: settings.Config{ServerPort: 1235, ProxyPort: 54321, LaunchText: "--arg"},
	}, func(context.Context, bool) error { return nil })
	if err == nil {
		t.Fatal("expected ConfigureLaunch to reject non-empty launchText")
	}

	// ConfigureLaunch: proxy-only rebind does not stop or restart engine.
	rebound := false
	cfgRes, err := e.ConfigureLaunch(context.Background(), settings.Configure{
		Engine:   "omlx",
		Settings: settings.Config{ServerPort: 1235, ProxyPort: 54321, LaunchText: ""},
	}, func(context.Context, bool) error {
		rebound = true
		return nil
	})
	if err != nil {
		t.Fatalf("ConfigureLaunch failed for proxyPort: %v", err)
	}
	if !rebound {
		t.Fatal("rebind callback was not invoked")
	}
	if cfgRes.ServerPort != 1235 {
		t.Fatalf("expected ServerPort=1235, got %d", cfgRes.ServerPort)
	}
	if cfgRes.LaunchText != "" {
		t.Fatalf("expected LaunchText=\"\", got %q", cfgRes.LaunchText)
	}
}

func TestSettingsRejectionOfManagedControls(t *testing.T) {
	// 1. Process mode (Ollama style): controls bind to OLLAMA_HOST
	t.Run("process mode host rejections", func(t *testing.T) {
		e := settingsExecutor(t, false)
		for _, input := range []string{
			"OLLAMA_HOST=0.0.0.0:11434",
			"OLLAMA_HOST=127.0.0.1:9999",
			"OLLAMA_HOST=:11434",
			"OLLAMA_HOST=192.168.1.50:11434",
		} {
			req := settingsRequest(t, e)
			req.Settings.LaunchText = input
			preview := previewSettings(t, e, req)
			if preview.Errors["launchText"] == "" {
				t.Fatalf("expected rejection for managed host: %s", input)
			}
		}
	})

	// 2. Command mode (LM Studio style): controls bind to --port, -p, --bind, LMS_SERVER_HOST
	t.Run("command mode flag and env rejections", func(t *testing.T) {
		e := settingsExecutor(t, true)
		for _, input := range []string{
			"--port 1234",
			"-p 1234",
			"-p1234",
			"-vp 1234",
			"-xyzp",
			"--bind 0.0.0.0",
			"--host 0.0.0.0",
			"LMS_SERVER_HOST=0.0.0.0",
		} {
			req := settingsRequest(t, e)
			req.Settings.LaunchText = input
			preview := previewSettings(t, e, req)
			if preview.Errors["launchText"] == "" {
				t.Fatalf("expected rejection for managed option: %s", input)
			}
		}
	})

	// 3. Shell operator and syntax rejections
	t.Run("shell syntax and quote rejections", func(t *testing.T) {
		e := settingsExecutor(t, false)
		for _, input := range []string{
			`"unclosed quote`,
			`'unclosed single quote`,
			`arg | other`,
			`arg && other`,
			`arg || other`,
			`arg ; other`,
			`arg > out`,
			`arg < in`,
			`arg $(whoami)`,
			"arg `whoami`",
		} {
			req := settingsRequest(t, e)
			req.Settings.LaunchText = input
			preview := previewSettings(t, e, req)
			if preview.Errors["launchText"] == "" {
				t.Fatalf("expected rejection for shell/syntax error: %s", input)
			}
		}
	})

	// 4. Duplicate environment variables
	t.Run("duplicate environment variables", func(t *testing.T) {
		e := settingsExecutor(t, false)
		req := settingsRequest(t, e)
		req.Settings.LaunchText = "CUSTOM_VAR=1 CUSTOM_VAR=2"
		preview := previewSettings(t, e, req)
		if preview.Errors["launchText"] == "" {
			t.Fatal("expected rejection for duplicate environment variable")
		}
	})

	// 5. Invalid port numbers
	t.Run("invalid port numbers", func(t *testing.T) {
		e := settingsExecutor(t, false)
		for _, badPort := range []int{0, -1, 65536, 100000} {
			req := settingsRequest(t, e)
			req.Settings.ServerPort = badPort
			req.Resolution = "server"
			preview := previewSettings(t, e, req)
			if preview.Errors["serverPort"] == "" {
				t.Fatalf("expected rejection for invalid port %d", badPort)
			}
		}
	})
}

func TestSettingsLANBindPreserved(t *testing.T) {
	// Verify applyLiteralLaunch accepts a non-loopback host when LAN bind is passed
	rt := Runtime{
		EditableLaunch: &EditableLaunch{
			FixedArgs: []string{"serve"},
			Controls: []LaunchControl{
				{Value: "{server.host}:{server.port}", Env: []string{"OLLAMA_HOST"}},
			},
		},
	}
	launch := launchCommand{
		Bin:  "/usr/bin/ollama",
		Args: []string{"serve"},
		Env:  map[string]string{},
	}
	vars := map[string]string{
		"host": "192.168.1.100",
		"port": "11434",
	}
	applied, err := applyLiteralLaunch(rt, launch, vars)
	if err != nil {
		t.Fatalf("applyLiteralLaunch failed on LAN bind address: %v", err)
	}
	if got := applied.Env["OLLAMA_HOST"]; got != "192.168.1.100:11434" {
		t.Fatalf("expected OLLAMA_HOST=192.168.1.100:11434, got %q", got)
	}
}
