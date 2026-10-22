// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Tests for the engine-native view the em surface serves: the engine's own
// counters and the engine's own live rows, mapped through the shared
// enginenative package.
//
// The security property under test is that the engine's API key cannot leave
// the node: the engine's own stats payload carries it, PAIR's type has no
// field for it, and the serialized answer must hold neither the field nor the
// value.
package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"nvpair-shared/clustertrust"
)

// omlxTestManifest returns a manifest for the omlx engine whose runtime port
// is port, so the executor's engine state points at a fake admin API.
func omlxTestManifest(port int) *Manifest {
	key := runtime.GOOS + "/" + runtime.GOARCH
	return &Manifest{
		Engine:          "omlx",
		DisplayName:     "oMLX",
		ManifestVersion: 1,
		Platforms: map[string]Platform{
			key: {
				Runtime: Runtime{Bin: "omlx", Port: port},
			},
		},
	}
}

// listenerPort returns the port a test server listens on.
func listenerPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	addr, ok := srv.Listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address type = %T, want *net.TCPAddr", srv.Listener.Addr())
	}
	return addr.Port
}

// closedPort returns a loopback port nothing listens on.
func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return port
}

// TestEngineNativeStatsReportsTheEngineView: a fake oMLX admin API answers
// the stats endpoint, and the executor reports the engine's own numbers —
// with the engine's api_key nowhere in the answer.
func TestEngineNativeStatsReportsTheEngineView(t *testing.T) {
	t.Setenv("OMLX_API_KEY", "sk-test-secret")
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_prompt_tokens":120,"total_completion_tokens":34,"total_cached_tokens":90,` +
			`"total_requests":2,"avg_prefill_tps":100,"avg_generation_tps":50,"cache_efficiency":75,` +
			`"active_models":["mlx-community/Llama-3.2-3B-Instruct-4bit"],` +
			`"api_key":"sk-test-secret","host":"127.0.0.1","port":8000,"cli_prefix":"omlx"}`))
	}))
	defer srv.Close()

	ex := newTestExecutor(t, omlxTestManifest(listenerPort(t, srv)))
	got := ex.EngineNativeStats(context.Background())

	if !got.Supported {
		t.Fatalf("supported = false, want true (%+v)", got)
	}
	if got.Error != "" {
		t.Fatalf("error = %q, want empty", got.Error)
	}
	if got.Engine != "omlx" {
		t.Fatalf("engine = %q, want omlx", got.Engine)
	}
	if got.Totals.Requests != 2 || got.Totals.PromptTokens != 120 || got.Totals.CompletionTokens != 34 {
		t.Fatalf("totals = %+v, want the engine's counters", got.Totals)
	}
	if got.Totals.CachedTokens != 90 {
		t.Fatalf("cached tokens = %d, want 90", got.Totals.CachedTokens)
	}
	if got.Totals.CacheEfficiency != 75 {
		t.Fatalf("cache efficiency = %v, want 75", got.Totals.CacheEfficiency)
	}
	if got.Rates.PrefillTokensPerSecond != 100 || got.Rates.GenerationTokensPerSecond != 50 {
		t.Fatalf("rates = %+v, want the engine's rates", got.Rates)
	}
	if len(got.Models) != 1 || got.Models[0].Model != "mlx-community/Llama-3.2-3B-Instruct-4bit" {
		t.Fatalf("models = %+v, want the engine's active model", got.Models)
	}
	if got.ObservedAt == 0 {
		t.Fatal("observedAt = 0, want the time the snapshot was taken")
	}
	if gotPath != "/admin/api/stats" {
		t.Fatalf("fetched path = %q, want /admin/api/stats", gotPath)
	}
	if gotAuth != "Bearer sk-test-secret" {
		t.Fatalf("authorization = %q, want the engine's key", gotAuth)
	}

	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "sk-test-secret") {
		t.Fatalf("the api key leaked into the answer: %s", body)
	}
	if strings.Contains(string(body), "api_key") {
		t.Fatalf("the api key field leaked into the answer: %s", body)
	}
}

// TestEngineNativeStatsReportsUnsupportedWhenTheEngineHasNoAdminAPI: a node
// with no omlx engine has no admin API to ask. That is a normal state, not a
// failure, so the answer is "unsupported" with no error.
func TestEngineNativeStatsReportsUnsupportedWhenTheEngineHasNoAdminAPI(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	got := ex.EngineNativeStats(context.Background())
	if got.Supported {
		t.Fatalf("supported = true, want false (%+v)", got)
	}
	if got.Error != "" {
		t.Fatalf("error = %q, want empty — an engine with no admin API is a normal state", got.Error)
	}
}

// TestEngineNativeStatsReportsUnsupportedWhenTheEngineHasNoAdminAPIEndpoint:
// an engine that answers "not found" is an engine that has no admin API — the
// build does not publish one, or it is disabled. The engine answered, so this
// is the normal unsupported state, and the answer carries no error.
func TestEngineNativeStatsReportsUnsupportedWhenTheEngineHasNoAdminAPIEndpoint(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusNotImplemented} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		ex := newTestExecutor(t, omlxTestManifest(listenerPort(t, srv)))
		got := ex.EngineNativeStats(context.Background())
		srv.Close()

		if got.Supported {
			t.Fatalf("HTTP %d: supported = true, want false (%+v)", status, got)
		}
		if got.Error != "" {
			t.Fatalf("HTTP %d: error = %q, want empty — an engine that answers %d has no admin API",
				status, got.Error, status)
		}
	}
}

// TestEngineNativeStatsReportsASanitisedErrorOnAFailingEngine: an engine
// that answers with a failing status is not an engine without an admin API —
// it is an engine whose admin API failed. The answer carries a short,
// sanitised error.
func TestEngineNativeStatsReportsASanitisedErrorOnAFailingEngine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ex := newTestExecutor(t, omlxTestManifest(listenerPort(t, srv)))
	got := ex.EngineNativeStats(context.Background())
	if got.Supported {
		t.Fatalf("supported = true, want false (%+v)", got)
	}
	if got.Error == "" {
		t.Fatal("error = empty, want a sanitised error")
	}
	if len(got.Error) > 120 {
		t.Fatalf("error = %q, want a short message", got.Error)
	}
	if strings.Contains(got.Error, "dial tcp") {
		t.Fatalf("the error leaked a transport detail: %q", got.Error)
	}
}

// TestEngineNativeStatsSurvivesAnUnreachableEngine: an engine that is not
// listening answers "unsupported" with a short, sanitised error — never a
// hang, never a crash, and never the engine's raw answer.
func TestEngineNativeStatsSurvivesAnUnreachableEngine(t *testing.T) {
	t.Setenv("OMLX_API_KEY", "sk-test-secret")
	port := closedPort(t)
	ex := newTestExecutor(t, omlxTestManifest(port))
	start := time.Now()
	got := ex.EngineNativeStats(context.Background())
	if time.Since(start) > 5*time.Second {
		t.Fatalf("the fetch took %s, want a bounded wait", time.Since(start))
	}
	if got.Supported {
		t.Fatalf("supported = true, want false (%+v)", got)
	}
	if got.Error != engineNativeUnreachable {
		t.Fatalf("error = %q, want the sanitised %q", got.Error, engineNativeUnreachable)
	}
	if len(got.Error) > 120 {
		t.Fatalf("error = %q, want a short message", got.Error)
	}
	if strings.Contains(got.Error, strconv.Itoa(port)) {
		t.Fatalf("the error leaked the port: %q", got.Error)
	}
	if strings.Contains(got.Error, "dial tcp") {
		t.Fatalf("the error leaked the transport error: %q", got.Error)
	}
	if strings.Contains(got.Error, "sk-test-secret") {
		t.Fatalf("the error leaked the api key: %q", got.Error)
	}
	if strings.Contains(got.Error, "api_key") {
		t.Fatalf("the error leaked the api key field: %q", got.Error)
	}
}

// TestEngineNativeStatsTimesOutOnASlowEngine: an engine that accepts the
// connection and never answers must not hold the caller: the fetch is
// bounded, and the answer says so.
func TestEngineNativeStatsTimesOutOnASlowEngine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer srv.Close()

	port := listenerPort(t, srv)
	ex := newTestExecutor(t, omlxTestManifest(port))
	ex.engineNativeTimeout = 100 * time.Millisecond
	start := time.Now()
	got := ex.EngineNativeStats(context.Background())
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("the fetch took %s, want the bounded timeout", elapsed)
	}
	if got.Supported {
		t.Fatalf("supported = true, want false (%+v)", got)
	}
	if got.Error != engineNativeTimedOut {
		t.Fatalf("error = %q, want the sanitised %q", got.Error, engineNativeTimedOut)
	}
	if strings.Contains(got.Error, strconv.Itoa(port)) {
		t.Fatalf("the error leaked the port: %q", got.Error)
	}
	if strings.Contains(got.Error, "dial tcp") {
		t.Fatalf("the error leaked the transport error: %q", got.Error)
	}
}

// TestEngineNativeActivityReportsTheEngineRows: the activity endpoint's rows
// come back as the engine reported them, phase by phase.
func TestEngineNativeActivityReportsTheEngineRows(t *testing.T) {
	t.Setenv("OMLX_API_KEY", "sk-test-secret")
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"active_models":[{"model":"m",` +
			`"waiting":[{"request_id":"r-wait","queue_position":2,"elapsed_seconds":1.5}],` +
			`"prefilling":[{"request_id":"r-prefill","progress":0.5}],` +
			`"generating":[{"request_id":"r-gen","generated_tokens":34,"tokens_per_second":62.1,` +
			`"elapsed_seconds":4.2,"prompt_tokens":1200}]}]}`))
	}))
	defer srv.Close()

	ex := newTestExecutor(t, omlxTestManifest(listenerPort(t, srv)))
	got := ex.EngineNativeActivity(context.Background())

	if !got.Supported {
		t.Fatalf("supported = false, want true (%+v)", got)
	}
	if got.Error != "" {
		t.Fatalf("error = %q, want empty", got.Error)
	}
	if got.Engine != "omlx" {
		t.Fatalf("engine = %q, want omlx", got.Engine)
	}
	if gotPath != "/admin/api/activity" {
		t.Fatalf("fetched path = %q, want /admin/api/activity", gotPath)
	}
	if len(got.Rows) != 3 {
		t.Fatalf("rows = %+v, want one row per phase", got.Rows)
	}
	byPhase := map[string]int{}
	for _, row := range got.Rows {
		byPhase[row.Phase]++
	}
	for _, phase := range []string{"waiting", "prefilling", "generating"} {
		if byPhase[phase] != 1 {
			t.Fatalf("phase %q count = %d, want 1 (%+v)", phase, byPhase[phase], got.Rows)
		}
	}
	for _, row := range got.Rows {
		if row.Phase != "generating" {
			continue
		}
		if row.RequestID != "r-gen" {
			t.Fatalf("request id = %q, want r-gen", row.RequestID)
		}
		if row.GeneratedTokens == nil || *row.GeneratedTokens != 34 {
			t.Fatalf("generated tokens = %+v, want 34", row.GeneratedTokens)
		}
		if row.TokensPerSecond == nil || *row.TokensPerSecond != 62.1 {
			t.Fatalf("tokens per second = %+v, want 62.1", row.TokensPerSecond)
		}
		if row.PromptTokens == nil || *row.PromptTokens != 1200 {
			t.Fatalf("prompt tokens = %+v, want 1200", row.PromptTokens)
		}
	}
}

// TestEngineNativeActivityReportsUnsupportedWhenTheEngineHasNoAdminAPI: the
// activity view follows the stats view — no admin API means "unsupported",
// with no error.
func TestEngineNativeActivityReportsUnsupportedWhenTheEngineHasNoAdminAPI(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	got := ex.EngineNativeActivity(context.Background())
	if got.Supported {
		t.Fatalf("supported = true, want false (%+v)", got)
	}
	if got.Error != "" {
		t.Fatalf("error = %q, want empty", got.Error)
	}
	if len(got.Rows) != 0 {
		t.Fatalf("rows = %+v, want none", got.Rows)
	}
}

// TestEngineNativeSurfaceIsGatedLikeTheModelList: the engine-native paths
// ride the em surface, so they are served on the same listener behind the
// same two gates — plaintext for loopback only, mTLS for pinned peers only.
func TestEngineNativeSurfaceIsGatedLikeTheModelList(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	plainMux, tlsMux := emSurface(ex, clustertrust.Open(t.TempDir()))

	for _, path := range []string{engineStatsPath, engineActivityPath} {
		// This node's own scanner, over loopback in plaintext.
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "127.0.0.1:51234"
		plainMux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s over loopback: code = %d, want 200", path, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != "application/json" {
			t.Fatalf("%s content type = %q, want application/json", path, got)
		}
		// The body is PAIR's own type: this executor knows no omlx engine, so
		// the answer is "unsupported" with no error.
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s body is not JSON: %v", path, err)
		}
		if body["supported"] != false {
			t.Fatalf("%s body = %+v, want supported:false", path, body)
		}
		if _, ok := body["error"]; ok {
			t.Fatalf("%s body = %+v, want no error", path, body)
		}

		// A LAN caller in the clear is refused, exactly like the model list.
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "192.168.1.42:51234"
		plainMux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s from a LAN caller: code = %d, want 403", path, rec.Code)
		}

		// The paths are GET-only.
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodPost, path, nil)
		req.RemoteAddr = "127.0.0.1:51234"
		plainMux.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s over POST: code = %d, want 405", path, rec.Code)
		}
	}

	// The mTLS personality admits no unpinned caller.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, engineStatsPath, nil)
	tlsMux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("%s over mTLS without a pin: code = %d, want 403", engineStatsPath, rec.Code)
	}
}

// TestTheLiveActivityPathIsNotServedToPeers: the engine's live rows are this
// node's own business — only this node's own proxy reads them, over
// loopback. The mTLS personality does not serve the path at all, so no peer
// can read another node's live engine rows.
func TestTheLiveActivityPathIsNotServedToPeers(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	plainMux, tlsMux := emSurface(ex, clustertrust.Open(t.TempDir()))

	// The local read still works: this node's own proxy reads it over
	// loopback.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, engineActivityPath, nil)
	req.RemoteAddr = "127.0.0.1:51234"
	plainMux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s over loopback: code = %d, want 200", engineActivityPath, rec.Code)
	}

	// A peer (or any other mTLS caller) gets no path at all.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, engineActivityPath, nil)
	tlsMux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("%s over mTLS: code = %d, want 404 (the path is not served to peers)", engineActivityPath, rec.Code)
	}
}
