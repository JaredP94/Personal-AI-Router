// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strconv"
	"testing"
)

func TestHTTPActionPathPlaceholders(t *testing.T) {
	var requestedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	port, _ := strconv.Atoi(u.Port())

	key := runtime.GOOS + "/" + runtime.GOARCH
	m := &Manifest{
		Engine:          "test-engine",
		DisplayName:     "Test Engine",
		ManifestVersion: 1,
		Platforms: map[string]Platform{
			key: {
				Runtime: Runtime{
					Port: port,
				},
			},
		},
		Actions: map[string]Action{
			"load_model": {
				HTTP: &ActionHTTP{
					Method: "POST",
					Path:   "/v1/models/{model}/load",
				},
			},
		},
	}

	ex := newTestExecutor(t, m)
	st, err := ex.state("test-engine")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	st.running = true
	st.port = port

	params := json.RawMessage(`{"model":"OsaurusAI--LFM2.5-8B-A1B-MXFP8"}`)
	res, err := ex.Action(context.Background(), "test-engine", "load_model", params)
	if err != nil {
		t.Fatalf("Action failed: %v", err)
	}

	expectedPath := "/v1/models/OsaurusAI--LFM2.5-8B-A1B-MXFP8/load"
	if requestedPath != expectedPath {
		t.Fatalf("expected path %q, got %q", expectedPath, requestedPath)
	}

	var parsed map[string]string
	if err := json.Unmarshal(res, &parsed); err != nil || parsed["status"] != "ok" {
		t.Fatalf("unexpected result: %s", string(res))
	}
}

func TestOMLXManifestActions(t *testing.T) {
	reg := NewRegistry()
	if err := reg.LoadFS(bundledManifests, "manifests"); err != nil {
		t.Fatalf("LoadFS: %v", err)
	}
	omlx, ok := reg.engines["omlx"]
	if !ok {
		t.Fatal("omlx manifest not found")
	}
	for _, action := range []string{"load_model", "unload_model"} {
		act, ok := omlx.Actions[action]
		if !ok {
			t.Fatalf("omlx manifest missing action %q", action)
		}
		if act.HTTP == nil {
			t.Fatalf("omlx action %q must be HTTP action", action)
		}
		if act.HTTP.Method != "POST" {
			t.Fatalf("omlx action %q method: expected POST, got %s", action, act.HTTP.Method)
		}
	}
}
