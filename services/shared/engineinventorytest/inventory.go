// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package engineinventorytest runs the real inventory producer against isolated
// fake engines. Only tests import it; no installed engines or user data are used.
package engineinventorytest

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Start returns the actual engine-manager inventory port, its private cache
// manifest, and an idempotent stop. The caller supplies the peer's pinned identity.
func Start(t *testing.T, clusterDir string) (int, string, func()) {
	t.Helper()
	root := t.TempDir()
	base := root
	if runtime.GOOS == "darwin" {
		base = filepath.Join(root, "Library", "Application Support")
	}
	app := filepath.Join(base, "Nvidia Corporation", "Personal AI Router")
	manifests := filepath.Join(app, "engines")
	if err := os.MkdirAll(manifests, 0700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, engine := range []string{"ollama", "lmstudio", "llamacpp", "omlx"} {
		var actions = map[string]any{}
		port := 0
		detect := filepath.Join(root, "not-installed")
		if engine == "ollama" || engine == "lmstudio" {
			name := engine + "-model"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/ready" {
					w.WriteHeader(http.StatusOK)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{{"name": name}}})
			}))
			t.Cleanup(srv.Close)
			_, p, _ := net.SplitHostPort(srv.Listener.Addr().String())
			port, _ = strconv.Atoi(p)
			detect = executable
			actions["list_models"] = map[string]any{"http": map[string]string{"method": "GET", "path": "/inventory"}, "result": map[string]string{"array": "models", "field": "name"}}
		}
		manifest := map[string]any{"engine": engine, "display_name": engine, "manifest_version": 1, "actions": actions,
			"platforms": map[string]any{runtime.GOOS + "/" + runtime.GOARCH: map[string]any{"detect": []string{detect}, "detect_requires": nil, "runtime": map[string]any{"mode": "process", "bin": detect, "port": port, "health": nil, "ready": map[string]any{"http": "http://127.0.0.1:{port}/ready", "status": 200, "json_match": nil}}}}}
		data, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(manifests, engine+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	cache := filepath.Join(app, "engine-bin", "llamacpp-models")
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	_, source, _, _ := runtime.Caller(0)
	binary := filepath.Join(root, "engine-manager")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	build.Dir = filepath.Join(filepath.Dir(source), "..", "..", "nvpair-engine-manager")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build inventory producer: %v\n%s", err, output)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	cmd := exec.CommandContext(t.Context(), binary, "--http-port", strconv.Itoa(port), "--cluster-dir", clusterDir, "--loaded-poll-interval", "0", "--log-level", "info")
	cmd.Env = os.Environ()
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "APPDATA", "LOCALAPPDATA", "USERPROFILE"} {
		for i := len(cmd.Env) - 1; i >= 0; i-- {
			if strings.HasPrefix(cmd.Env[i], key+"=") {
				cmd.Env = append(cmd.Env[:i], cmd.Env[i+1:]...)
			}
		}
		cmd.Env = append(cmd.Env, key+"="+root)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = io.Discard
	log, err := os.Create(filepath.Join(root, "producer.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = stdin.Close()
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
			_ = log.Close()
		})
	}
	t.Cleanup(stop)
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/models", nil)
		resp, err := client.Do(req)
		if err == nil {
			var body struct {
				Models []string `json:"models"`
			}
			decodeErr := json.NewDecoder(resp.Body).Decode(&body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK && decodeErr == nil && len(body.Models) == 2 {
				break
			}
		}
		select {
		case <-ctx.Done():
			bytes, _ := os.ReadFile(filepath.Join(root, "producer.log"))
			t.Fatalf("inventory fixture not ready: %v\n%s", ctx.Err(), bytes)
		case <-time.After(20 * time.Millisecond):
		}
	}
	return port, filepath.Join(cache, ".pair-models.json"), stop
}
