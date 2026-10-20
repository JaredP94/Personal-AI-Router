// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func resolutionRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "LOCALAPPDATA", "XDG_CONFIG_HOME"} {
		t.Setenv(key, root)
	}
	_, base := userPaths()
	return base
}

func resolutionFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// Not a runnable program: resolution must only inspect files, never exec them.
	if err := os.WriteFile(path, []byte("not an executable\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func resolutionRegistry(t *testing.T, manifest string) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := reg.LoadFS(fstest.MapFS{"manifests/test.json": &fstest.MapFile{Data: []byte(manifest)}}, "manifests"); err != nil {
		t.Fatal(err)
	}
	return reg
}

const resolutionManifest = `{
  "engine":"test-engine", "display_name":"Test Engine", "manifest_version":1,
  "detect":["{install_dir}/first", "{install_dir}/second"],
  "runtime":{"bin":"{install_dir}/not-the-detected-binary", "port":11435,
    "bind":"0.0.0.0", "env":{"OLLAMA_HOST":"{host}:{port}"}},
  "platforms":{"linux/amd64":{"runtime":{"env":{"LD_LIBRARY_PATH":"{install_dir}/lib/ollama"}}}}
}`

func TestResolveEngineProcessUsesFirstDetectionAndRuntimeEnv(t *testing.T) {
	base := resolutionRoot(t)
	first := filepath.Join(base, "test-engine") + "/first"
	resolutionFile(t, first)
	resolutionFile(t, filepath.Join(base, "test-engine", "second"))
	reg := resolutionRegistry(t, resolutionManifest)
	t.Setenv("OLLAMA_HOST", "127.0.0.1:11434")
	got, err := resolveEngine(reg, "test-engine", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	wantEnv := map[string]string{
		"OLLAMA_HOST":     "127.0.0.1:11435",
		"LD_LIBRARY_PATH": filepath.Join(base, "test-engine") + "/lib/ollama",
	}
	if got.Engine != "test-engine" || got.Path != first || !reflect.DeepEqual(got.Env, wantEnv) {
		t.Fatalf("resolution = %+v, want path %q and env %v", got, first, wantEnv)
	}
	if os.Getenv("OLLAMA_HOST") != "127.0.0.1:11434" {
		t.Fatal("resolution changed the caller's environment")
	}
}

func TestResolveEngineHonorsPersistedPortAndCustomPathOverride(t *testing.T) {
	resolutionRoot(t)
	reg := resolutionRegistry(t, resolutionManifest)
	custom := filepath.Join(t.TempDir(), "custom engine")
	resolutionFile(t, custom)
	dir, _ := userPaths()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"engine": "test-engine", "detect": []string{custom}, "runtime": map[string]int{"port": 23456}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "test-engine.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reg.LoadOverrideDir(dir); err != nil {
		t.Fatal(err)
	}
	got, err := resolveEngine(reg, "test-engine", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != custom || got.Env["OLLAMA_HOST"] != "127.0.0.1:23456" || got.Env["LD_LIBRARY_PATH"] == "" {
		t.Fatalf("override lost or defaults not merged: %+v", got)
	}
}

func TestResolveEngineCommandUsesRuntimeCLIAndRequiresItOnDisk(t *testing.T) {
	base := resolutionRoot(t)
	reg := resolutionRegistry(t, `{
	  "engine":"test-engine", "display_name":"Test Engine", "manifest_version":1,
	  "detect":["{install_dir}/alternate"],
	  "runtime":{"mode":"command", "cli":"~/control-cli", "port":1235,
	    "start":[["{cli}","start"]]}, "platforms":{"linux/amd64":{}}
	}`)
	resolutionFile(t, filepath.Join(base, "test-engine", "alternate"))
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(home, "control-cli")
	resolutionFile(t, cli)
	got, err := resolveEngine(reg, "test-engine", "linux", "amd64")
	if err != nil || got.Path != cli || got.Env == nil || len(got.Env) != 0 {
		t.Fatalf("command resolution = %+v, %v", got, err)
	}
	if err := os.Remove(cli); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveEngine(reg, "test-engine", "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "is not installed") {
		t.Fatalf("missing CLI must not fall back to detect path: %v", err)
	}
}

func TestResolveEngineErrors(t *testing.T) {
	resolutionRoot(t)
	reg := resolutionRegistry(t, resolutionManifest)
	for _, tc := range []struct{ engine, goos, want string }{
		{"missing", "linux", "unknown engine"},
		{"test-engine", "windows", "is not supported on windows/amd64"},
		{"test-engine", "linux", "is not installed"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			if _, err := resolveEngine(reg, tc.engine, tc.goos, "amd64"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func externalResolutionRegistry(t *testing.T, address, mode string) *Registry {
	t.Helper()
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	return resolutionRegistry(t, fmt.Sprintf(`{
	 "engine":"external", "display_name":"External", "manifest_version":1,
	 "detect":["{install_dir}/missing"],
	 "runtime":{"mode":%q,"bin":"{install_dir}/missing","cli":"{install_dir}/missing",
	   "start":[["{cli}","start"]], "port":%s,
	   "ready":{"http":"http://%s:{port}/ready","timeout_s":600}},
	 "platforms":{"linux/amd64":{}}
	}`, mode, port, host))
}

func TestResolveEngineExternalServiceWithoutBinary(t *testing.T) {
	resolutionRoot(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ready" || r.Header.Get(engineIdentityProbeHeader) != "1" {
			t.Errorf("unexpected identity probe: %s, headers %v", r.URL, r.Header)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	reg := externalResolutionRegistry(t, srv.Listener.Addr().String(), "process")
	_, err := resolveEngine(reg, "external", "linux", "amd64")
	if err == nil || err.Error() != "external is running as an external service; PAIR has no executable for it" {
		t.Fatalf("external error = %v", err)
	}
	reg = externalResolutionRegistry(t, srv.Listener.Addr().String(), "command")
	if _, err := resolveEngine(reg, "external", "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "is not installed") {
		t.Fatalf("command mode must not adopt HTTP-only service: %v", err)
	}
}

func TestResolveEngineExternalProbeIsBoundedAndDoesNotFollowRedirects(t *testing.T) {
	resolutionRoot(t)
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("one-shot identity probe followed an HTTP redirect")
		w.WriteHeader(http.StatusOK)
	}))
	defer redirectTarget.Close()
	for _, behavior := range []string{"slow", "redirect", "unhealthy"} {
		t.Run(behavior, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch behavior {
				case "slow":
					<-r.Context().Done()
				case "redirect":
					http.Redirect(w, r, redirectTarget.URL, http.StatusFound)
				default:
					w.WriteHeader(http.StatusServiceUnavailable)
				}
			}))
			defer srv.Close()
			reg := externalResolutionRegistry(t, srv.Listener.Addr().String(), "process")
			start := time.Now()
			_, err := resolveEngine(reg, "external", "linux", "amd64")
			if err == nil || !strings.Contains(err.Error(), "is not installed") {
				t.Fatalf("indeterminate service must not be adopted: %v", err)
			}
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("probe took %v despite long manifest timeout", elapsed)
			}
		})
	}
}

func TestResolveEngineDoesNotProbeNonLoopback(t *testing.T) {
	resolutionRoot(t)
	reg := externalResolutionRegistry(t, "192.0.2.1:54321", "process")
	start := time.Now()
	if _, err := resolveEngine(reg, "external", "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "is not installed") {
		t.Fatalf("non-loopback service error = %v", err)
	}
	if time.Since(start) > 250*time.Millisecond {
		t.Fatal("non-loopback probe should have been skipped")
	}
}

func TestListEnginesIncludesUnsupportedMissingAndExternal(t *testing.T) {
	base := resolutionRoot(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	reg := externalResolutionRegistry(t, srv.Listener.Addr().String(), "process")
	if _, err := reg.addManifest("process", []byte(resolutionManifest)); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.addManifest("unsupported", []byte(`{
	  "engine":"unsupported", "display_name":"Unsupported", "manifest_version":1,
	  "runtime":{"bin":"{install_dir}/missing"}, "platforms":{"windows/amd64":{}}
	}`)); err != nil {
		t.Fatal(err)
	}
	want := []listedEngine{
		{Engine: "external", DisplayName: "External", Installed: true, Supported: true},
		{Engine: "test-engine", DisplayName: "Test Engine", Installed: false, Supported: true},
		{Engine: "unsupported", DisplayName: "Unsupported", Installed: false, Supported: false},
	}
	if got := listEngines(reg, "linux", "amd64"); !reflect.DeepEqual(got, want) {
		t.Fatalf("list = %+v, want %+v", got, want)
	}
	resolutionFile(t, filepath.Join(base, "test-engine", "first"))
	want[1].Installed = true
	if got := listEngines(reg, "linux", "amd64"); !reflect.DeepEqual(got, want) {
		t.Fatalf("filesystem installation not reflected: %+v", got)
	}
	if got := listEngines(NewRegistry(), "linux", "amd64"); got == nil || len(got) != 0 {
		t.Fatalf("empty registry must encode as [], not null: %+v", got)
	}
}

func TestOneShotCLI(t *testing.T) {
	program := filepath.Join(t.TempDir(), "engine-manager")
	if runtime.GOOS == "windows" {
		program += ".exe"
	}
	build := exec.Command("go", "build", "-o", program, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	base := resolutionRoot(t)
	manifestDir, _ := userPaths()
	if err := os.MkdirAll(manifestDir, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(base, "test-engine") + "/first"
	resolutionFile(t, binary)
	manifest := strings.ReplaceAll(resolutionManifest, "linux/amd64", runtime.GOOS+"/"+runtime.GOARCH)
	if err := os.WriteFile(filepath.Join(manifestDir, "test-engine.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	before := resolutionTree(t, filepath.Dir(base))
	run := func(args ...string) (string, string, int) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, program, args...)
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		// A real open stdin pipe catches accidentally entering the stdio service.
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		defer stdin.Close()
		if err := cmd.Run(); err != nil && ctx.Err() != nil {
			t.Fatalf("one-shot did not exit: %v", err)
		}
		return out.String(), stderr.String(), cmd.ProcessState.ExitCode()
	}
	t.Run("list shape", func(t *testing.T) {
		out, stderr, code := run("--log-level", "debug", "--list-engines")
		if code != 0 {
			t.Fatalf("exit %d, stdout=%s stderr=%s", code, out, stderr)
		}
		var entries []map[string]json.RawMessage
		if err := json.Unmarshal([]byte(out), &entries); err != nil || len(entries) < 4 {
			t.Fatalf("list is not a JSON array of bundled + user engines: %q, %v", out, err)
		}
		found := false
		for _, entry := range entries {
			if len(entry) != 4 || entry["engine"] == nil || entry["display_name"] == nil || entry["installed"] == nil || entry["supported"] == nil {
				t.Fatalf("wrong list contract: %s", out)
			}
			if string(entry["engine"]) == `"test-engine"` {
				found = true
				if string(entry["installed"]) != "true" || string(entry["supported"]) != "true" || string(entry["display_name"]) != `"Test Engine"` {
					t.Fatalf("wrong custom engine entry: %v", entry)
				}
			}
		}
		if !found {
			t.Fatal("custom engine missing from list")
		}
	})
	t.Run("resolve JSON", func(t *testing.T) {
		out, stderr, code := run("--resolve", "test-engine", "--log-level=warn")
		var result struct {
			Engine string            `json:"engine"`
			Path   string            `json:"path"`
			Env    map[string]string `json:"env"`
		}
		if err := json.Unmarshal([]byte(out), &result); code != 0 || err != nil || result.Engine != "test-engine" || result.Path != binary || result.Env["OLLAMA_HOST"] != "127.0.0.1:11435" {
			t.Fatalf("resolve: code=%d stdout=%q stderr=%q error=%v", code, out, stderr, err)
		}
	})
	for _, args := range [][]string{
		{"--resolve", "missing"}, {"--resolve"}, {"--resolve="},
		{"--resolve", "test-engine", "--list-engines"},
		{"--list-engines", "--ipc", "unused"}, {"--list-engines", "--http-port", "12345"},
		{"--resolve", "test-engine", "--cluster-dir", "unused"},
		{"--list-engines", "--version"}, {"--list-engines", "extra"},
		{"--ipc", "unused", "--resolve", "test-engine"},
		{"--list-engines", "--control-port=12345"}, {"--list-engines", "--reserved-port=12345"},
		{"--list-engines", "--loaded-poll-interval=1"}, {"--list-engines=false"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			out, stderr, code := run(args...)
			if code != 2 || out != "" || len(strings.Split(strings.TrimSpace(stderr), "\n")) != 1 || strings.TrimSpace(stderr) == "" {
				t.Fatalf("want exit 2, no stdout, one stderr line: code=%d stdout=%q stderr=%q", code, out, stderr)
			}
		})
	}
	t.Run("malformed override diagnostics", func(t *testing.T) {
		broken := filepath.Join(manifestDir, "ollama.json")
		if err := os.WriteFile(broken, []byte(`{"engine":"ollama",`), 0o600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(broken)
		withBroken := resolutionTree(t, filepath.Dir(base))
		t.Run("invalid explicit log level", func(t *testing.T) {
			t.Setenv("NVPAIR_LOG_LEVEL", "debug")
			for _, arg := range []string{"--log-level=", "--log-level=invalid"} {
				for _, mode := range [][]string{
					{"--resolve", "test-engine"}, {"--list-engines"}, {"--resolve", "missing"},
				} {
					t.Run(strings.Join(mode, " ")+" "+arg, func(t *testing.T) {
						out, stderr, code := run(append(mode, arg)...)
						if code != 2 || out != "" || strings.Count(stderr, "\n") != 1 || !strings.Contains(stderr, "--log-level") {
							t.Fatalf("invalid flag must be a one-line usage error, not environment debug: code=%d stdout=%q stderr=%q", code, out, stderr)
						}
					})
				}
			}
		})
		for _, level := range []struct {
			name, env string
			args      []string
			debug     bool
		}{
			{name: "default", env: "info"},
			{name: "environment debug only", env: "debug"},
			{name: "explicit info", env: "debug", args: []string{"--log-level=info"}},
			{name: "explicit warn", env: "debug", args: []string{"--log-level=warn"}},
			{name: "explicit error", env: "debug", args: []string{"--log-level=error"}},
			{name: "explicit debug", env: "info", args: []string{"--log-level=debug"}, debug: true},
		} {
			t.Run(level.name, func(t *testing.T) {
				t.Setenv("NVPAIR_LOG_LEVEL", level.env)
				for _, mode := range []struct {
					name string
					args []string
					code int
				}{
					{name: "resolve", args: []string{"--resolve", "test-engine"}},
					{name: "list", args: []string{"--list-engines"}},
					{name: "unknown", args: []string{"--resolve", "missing"}, code: 2},
				} {
					t.Run(mode.name, func(t *testing.T) {
						out, stderr, code := run(append(mode.args, level.args...)...)
						if code != mode.code {
							t.Fatalf("exit=%d, want %d; stdout=%q stderr=%q", code, mode.code, out, stderr)
						}
						if level.debug {
							if !strings.Contains(stderr, "skipping invalid override manifest") {
								t.Fatalf("explicit debug lost loader diagnostic: %q", stderr)
							}
						} else if mode.code == 0 && stderr != "" {
							t.Fatalf("successful one-shot leaked diagnostics: %q", stderr)
						} else if mode.code != 0 && (strings.Count(stderr, "\n") != 1 || !strings.HasPrefix(stderr, "unknown engine")) {
							t.Fatalf("failure must contain only one reason line: %q", stderr)
						}
						switch mode.name {
						case "resolve":
							var result Resolution
							if err := json.Unmarshal([]byte(out), &result); err != nil || result.Path != binary {
								t.Fatalf("malformed override broke valid resolution: %q, %v", out, err)
							}
						case "list":
							var entries []listedEngine
							if err := json.Unmarshal([]byte(out), &entries); err != nil {
								t.Fatal(err)
							}
							found := false
							for _, entry := range entries {
								found = found || entry.Engine == "ollama"
							}
							if !found {
								t.Fatal("malformed override removed the bundled engine")
							}
						case "unknown":
							if out != "" || !strings.Contains(stderr, "unknown engine") {
								t.Fatalf("unknown engine: stdout=%q stderr=%q", out, stderr)
							}
						}
					})
				}
			})
		}
		if after := resolutionTree(t, filepath.Dir(base)); !reflect.DeepEqual(withBroken, after) {
			t.Fatalf("one-shot modified malformed override tree: before=%v after=%v", withBroken, after)
		}
	})
	if after := resolutionTree(t, filepath.Dir(base)); !reflect.DeepEqual(before, after) {
		t.Fatalf("one-shot modified config tree: before=%v after=%v", before, after)
	}
}

func resolutionTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[path] = string(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
