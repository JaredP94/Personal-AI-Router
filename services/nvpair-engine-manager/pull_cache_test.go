// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOllamaCancelDeletesNothingFromEngineCache(t *testing.T) {
	root := t.TempDir()
	blobs := filepath.Join(root, "blobs")
	if err := os.Mkdir(blobs, 0700); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	names := []string{"sha256-" + digest + "-partial-0", "sha256-" + digest, "unrelated-partial"}
	for _, name := range names[1:] {
		if err := os.WriteFile(filepath.Join(blobs, name), []byte("shared data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("OLLAMA_MODELS", root)
	started, engineStopped := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Appearing after our pull starts does not make shared data ours.
		if err := os.WriteFile(filepath.Join(blobs, names[0]), []byte("shared data"), 0600); err != nil {
			t.Error(err)
			return
		}
		fmt.Fprintf(w, `{"status":"pulling","digest":"sha256:%s","completed":1,"total":100}`+"\n", digest)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(engineStopped)
	}))
	defer srv.Close()
	manifest := &Manifest{Engine: "ollama", Actions: map[string]Action{pullModelAction: {HTTP: &ActionHTTP{Method: "POST", Path: "/api/pull"}}}}
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	ex.engines["ollama"] = &engineState{manifest: manifest, plat: &Platform{}, running: true, port: portOf(t, srv.URL), modelsDir: root}
	done := make(chan error, 1)
	go func() {
		_, err := ex.PullModelStream(context.Background(), "ollama", "a", nil, "cache-op")
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("pull did not start")
	}
	if !ex.CancelModelPull("ollama", "a", "cache-op") {
		t.Fatal("cancel not accepted")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("pull not stopped")
	}
	select {
	case <-engineStopped:
	case <-time.After(time.Second):
		t.Fatal("engine connection still open")
	}
	entries, err := os.ReadDir(blobs)
	if err != nil || len(entries) != len(names) {
		t.Fatalf("cache entries changed: %v %v", entries, err)
	}
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(blobs, name))
		if err != nil || string(b) != "shared data" {
			t.Fatalf("cache changed at %s: %q %v", name, b, err)
		}
	}
}
