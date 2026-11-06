// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func llamaManifest(t *testing.T) *Manifest {
	t.Helper()
	m := bundledManifestSet(t)["llamacpp"]
	if m == nil {
		t.Fatal("llama.cpp manifest missing")
	}
	return m
}

func TestLlamaManifestLifecycleOnly(t *testing.T) {
	m := llamaManifest(t)
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(m.Platforms) != 1 {
		t.Fatal("only macOS arm64 supported")
	}
	p, ok := m.PlatformFor("darwin", "arm64")
	if !ok {
		t.Fatal("macOS arm64 missing")
	}
	if p.Install.Fetch.SHA256 != "870ef967970fa18bbd31460bc5f7929bbb5f091359d8dc0f58e20133aa4b5fcd" || !strings.HasSuffix(p.Install.Fetch.URL, "/b12171/llama-b12171-bin-macos-arm64.tar.gz") {
		t.Fatal("unverified release")
	}
	if len(m.Actions) != 0 {
		t.Fatal("model integration must remain deferred")
	}
	if p.Runtime.Port != 8082 || p.Runtime.Bind != "127.0.0.1" || p.Runtime.Stop.WaitForExit {
		t.Fatal("unsafe lifecycle defaults")
	}
	if p.Runtime.Ready.JSONMatch == nil || p.Runtime.Ready.JSONMatch.Field != "role" || p.Runtime.Ready.JSONMatch.Value != "router" {
		t.Fatal("identity probe missing")
	}
	for _, key := range []string{"linux/amd64", "linux/arm64", "windows/amd64", "windows/arm64", "darwin/amd64"} {
		parts := strings.Split(key, "/")
		if _, ok := m.PlatformFor(parts[0], parts[1]); ok {
			t.Fatal("unsupported platform enabled")
		}
	}
}

func TestLlamaProtectedLaunchInputs(t *testing.T) {
	p, _ := llamaManifest(t).PlatformFor("darwin", "arm64")
	flags := []string{"--host", "--port", "--reuse-port", "--embd-gemma-default", "--fim-qwen-1.5b-default", "--fim-qwen-3b-default", "--fim-qwen-7b-default", "--fim-qwen-7b-spec", "--fim-qwen-14b-spec", "--fim-qwen-30b-default", "--gpt-oss-20b-default", "--gpt-oss-120b-default", "--vision-gemma-4b-default", "--vision-gemma-12b-default"}
	for _, flag := range flags {
		for _, spelling := range []string{flag, strings.ReplaceAll(flag, "-", "_")} {
			// Only underscores after the leading -- are accepted by the vendor parser.
			spelling = "--" + strings.TrimLeft(spelling, "-_")
			for _, input := range []string{spelling + " 8082", spelling + "=8082", spelling + " 8082 " + spelling + " 8082", "-- " + spelling + " 8082"} {
				t.Run(input, func(t *testing.T) {
					ex := settingsExecutor(t, true)
					settingsState(t, ex).plat.Runtime = p.Runtime
					req := settingsRequest(t, ex)
					req.Settings.LaunchText = input
					if len(previewSettings(t, ex, req).Errors) == 0 {
						t.Fatalf("accepted %s", input)
					}
				})
			}
		}
	}
	for _, key := range []string{"LLAMA_ARG_HOST", "LLAMA_ARG_PORT", "LLAMA_ARG_REUSE_PORT", "LLAMA_CACHE"} {
		for _, input := range []string{key + "=127.0.0.1", key + "=8082 " + key + "=8082", "-- " + key + "=8082"} {
			t.Run(input, func(t *testing.T) {
				ex := settingsExecutor(t, true)
				settingsState(t, ex).plat.Runtime = p.Runtime
				req := settingsRequest(t, ex)
				req.Settings.LaunchText = input
				if len(previewSettings(t, ex, req).Errors) == 0 {
					t.Fatalf("accepted %s", input)
				}
			})
		}
	}
	// Persisted overrides must not be a second, less strict launch path.
	for _, flag := range flags {
		rt := p.Runtime
		args := []string{"--", flag + "=8082"}
		rt.LaunchArgs = &args
		if _, err := resolveProcessLaunch(rt, "/fixture/server", map[string]string{"host": "127.0.0.1", "port": "8082", "install_dir": "/fixture"}); err == nil {
			t.Fatalf("persisted %s accepted", flag)
		}
	}
}

func TestLlamaResolvedLaunch(t *testing.T) {
	p, _ := llamaManifest(t).PlatformFor("darwin", "arm64")
	t.Setenv("LLAMA_ARG_HOST", "/tmp/unwanted.sock")
	t.Setenv("LLAMA_ARG_PORT", "9999")
	t.Setenv("LLAMA_ARG_REUSE_PORT", "1")
	t.Setenv("LLAMA_CACHE", "/tmp/unwanted-cache")
	args, env := []string{"--threads", "4"}, []string{"CUSTOM=literal"}
	p.Runtime.LaunchArgs = &args
	p.Runtime.LaunchEnv = &env
	cmd, err := resolveProcessLaunch(p.Runtime, "/fixture/llama-server", map[string]string{"host": "127.0.0.1", "port": "8082", "install_dir": "/fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cmd.Args, " ") != "--sleep-idle-seconds 300 --host 127.0.0.1 --port 8082 --threads 4" || cmd.Env["LLAMA_CACHE"] != "/fixture-models" || cmd.Env["LLAMA_ARG_HOST"] != "127.0.0.1" || cmd.Env["LLAMA_ARG_PORT"] != "8082" || cmd.Env["LLAMA_ARG_REUSE_PORT"] != "0" {
		t.Fatalf("unsafe launch: %+v", cmd)
	}
}

func TestLlamaReadinessIdentity(t *testing.T) {
	p, _ := llamaManifest(t).PlatformFor("darwin", "arm64")
	for _, body := range []string{`{"role":"router"}`, `{}`, `{"role":"model"}`, `{"role":true}`, `not json`, `{"role":"router"} trailing`, strings.Repeat(" ", 65537) + `{"role":"router"}`} {
		t.Run(body[:min(30, len(body))], func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/props" {
					t.Error("wrong identity path")
				}
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
			if got := ex.probe(context.Background(), p.Runtime.Ready, portOf(t, srv.URL)); got != (body == `{"role":"router"}`) {
				t.Fatalf("identity accepted=%v", got)
			}
		})
	}
}

func TestLlamaNoProxyLaunchSettings(t *testing.T) {
	p, _ := llamaManifest(t).PlatformFor("darwin", "arm64")
	ex := settingsExecutor(t, true)
	st := settingsState(t, ex)
	st.plat.Runtime = p.Runtime
	ex.engines["llamacpp"] = st
	request := settingsRequest(t, ex)
	request.Engine = "llamacpp"
	request.Settings.ServerPort = 8082
	request.Settings.ProxyPort = 0
	request.Settings.LaunchText = "CUSTOM=value --threads 4"
	preview, err := ex.PreviewLaunch(request)
	if err != nil || len(preview.Errors) != 0 || preview.Settings.ProxyPort != 0 {
		t.Fatalf("proxy-free preview: %+v %v", preview, err)
	}
	request.Settings.ProxyPort = 8081
	preview, err = ex.PreviewLaunch(request)
	if err == nil && len(preview.Errors) == 0 {
		t.Fatal("future proxy accepted")
	}
}

func TestLlamaPortCollision(t *testing.T) {
	m := llamaManifest(t)
	p, _ := m.PlatformFor("darwin", "arm64")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"role":"other"}`)) }))
	defer srv.Close()
	reg := NewRegistry()
	reg.engines["llamacpp"] = m
	ex := reconciledTestExecutor(NewExecutor(reg, NewReporter(nil), nil, t.TempDir()))
	dir := filepath.Join(t.TempDir(), "engine")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "llama-server"), []byte("not executable fixture"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"libllama-server-impl.dylib", "libggml-metal.0.29.0.dylib"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	p.Runtime.Port = portOf(t, srv.URL)
	ex.engines["llamacpp"] = &engineState{manifest: m, plat: p, port: p.Runtime.Port, installDir: dir, logs: newLogBuffer()}
	if err := ex.Start(context.Background(), "llamacpp"); err == nil || !strings.Contains(err.Error(), "occupied") {
		t.Fatalf("collision: %v", err)
	}
}
