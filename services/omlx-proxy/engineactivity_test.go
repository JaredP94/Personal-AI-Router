// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"nvpair-shared/enginenative"
)

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestForwardedPeerRequestCarriesTheJobId: a request forwarded to a cluster
// peer carries PAIR's job id, so the serving peer can pair the engine's
// request id with it. A request that is not forwarded to a peer does not
// carry it — the header exists for the peer that serves the request.
func TestForwardedPeerRequestCarriesTheJobId(t *testing.T) {
	p := testProxy(NewDiscovery(), 1235)

	peer := candidate{
		id:       "peer-a",
		url:      &url.URL{Scheme: "https", Host: "192.0.2.10:1234"},
		peerUUID: "principal-peer",
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	p.forwardDirector(peer, "42")(req)
	if got := req.Header.Get(nvpairRequestIDHeader); got != "42" {
		t.Fatalf("%s = %q, want the job id 42", nvpairRequestIDHeader, got)
	}

	local := candidate{
		id:  "local-node",
		url: &url.URL{Scheme: "http", Host: "127.0.0.1:8000"},
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	p.forwardDirector(local, "42")(req)
	if got := req.Header.Get(nvpairRequestIDHeader); got != "" {
		t.Fatalf("%s = %q, want it absent on a request that is not forwarded to a peer", nvpairRequestIDHeader, got)
	}
}

// TestIngressPairsTheEngineRequestIdWithTheJobId: the ingress reads PAIR's
// job id from the forwarded request and pairs the engine's own request id
// with it, so the engine's live rows can be published already keyed by that
// job id. The pairing dies with the request.
func TestIngressPairsTheEngineRequestIdWithTheJobId(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	releaseEngine := func() { once.Do(func() { close(release) }) }
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(engineRequestIDHeader, "eng-123")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
	}))
	defer engine.Close()
	defer releaseEngine()

	p := testProxy(NewDiscovery(), 1235)
	target, err := url.Parse(engine.URL)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		req.Header.Set(nvpairRequestIDHeader, "job-42")
		p.serveLocalEngine(httptest.NewRecorder(), req, target, req.Header.Get(nvpairRequestIDHeader))
	}()

	waitFor(t, "the engine request id to be paired with the job id", func() bool {
		jobID, ok := p.engineJobID("eng-123")
		return ok && jobID == "job-42"
	})

	releaseEngine()
	<-done

	if _, ok := p.engineJobID("eng-123"); ok {
		t.Fatal("the pairing outlived the request")
	}
}

// TestLocalRequestPairsTheEngineRequestIdWithTheJobId: a request this proxy
// serves itself is paired the same way — the engine's own request id with
// PAIR's job id — so the engine's live rows are published under the job id
// the rest of the cluster uses.
func TestLocalRequestPairsTheEngineRequestIdWithTheJobId(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	releaseEngine := func() { once.Do(func() { close(release) }) }
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(engineRequestIDHeader, "eng-456")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
	}))
	defer engine.Close()
	defer releaseEngine()

	rec := &prRec{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "serving-node", engine.URL, "llama"))
	p := NewProxy(NewCodec(rec), disc, 1235)

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.handleHTTP(httptest.NewRecorder(),
			httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"llama"}`)))
	}()

	var jobID string
	waitFor(t, "the engine request id to be paired with the job id", func() bool {
		id, ok := p.engineJobID("eng-456")
		if ok {
			jobID = id
		}
		return ok
	})

	// The paired job id is the proxy's own request id — the same id the
	// workload lifecycle events carry.
	workloadID := ""
	for _, note := range proxyNotifications(t, rec) {
		if note.Method != workloadStartedMethod {
			continue
		}
		var params struct {
			WorkloadInfo struct {
				ID string `json:"id"`
			} `json:"workloadInfo"`
		}
		if err := json.Unmarshal(note.Params, &params); err != nil {
			t.Fatal(err)
		}
		workloadID = params.WorkloadInfo.ID
	}
	if workloadID == "" {
		t.Fatal("no workload:started event was emitted")
	}
	if jobID != workloadID {
		t.Fatalf("the paired job id = %q, want the workload's id %q", jobID, workloadID)
	}

	releaseEngine()
	<-done
}

// fakeEngineActivity answers the em surface's engine-activity endpoint with
// the given rows.
func fakeEngineActivity(t *testing.T, rows []enginenative.ActivityRow) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/engine-activity" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(enginenative.Activity{
			Engine:    enginenative.EngineOmlx,
			Supported: true,
			Rows:      rows,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestPublishedEngineRowsCarryOnlyReportedValues: the engine's live rows
// reach the frame with the readings the engine reported and nothing else. A
// reading the engine reported as zero stays a zero; a reading it did not
// report stays absent rather than becoming a zero.
func TestPublishedEngineRowsCarryOnlyReportedValues(t *testing.T) {
	restoreInterval := progressInterval
	progressInterval = 20 * time.Millisecond
	defer func() { progressInterval = restoreInterval }()
	restorePoll := engineActivityPollInterval
	engineActivityPollInterval = 20 * time.Millisecond
	defer func() { engineActivityPollInterval = restorePoll }()

	em := fakeEngineActivity(t, []enginenative.ActivityRow{{
		RequestID:       "eng-123",
		Phase:           enginenative.PhaseGenerating,
		GeneratedTokens: intPtr(0),
	}})
	restoreURL := engineManagerActivityURL
	engineManagerActivityURL = em.URL + "/v1/engine-activity"
	defer func() { engineManagerActivityURL = restoreURL }()

	release := make(chan struct{})
	var once sync.Once
	releaseEngine := func() { once.Do(func() { close(release) }) }
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(engineRequestIDHeader, "eng-123")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
	}))
	defer engine.Close()
	defer releaseEngine()

	rec := &prRec{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "serving-node", engine.URL, "llama"))
	p := NewProxy(NewCodec(rec), disc, 1235)
	p.nodeID = "node-self"

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.handleHTTP(httptest.NewRecorder(),
			httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"llama"}`)))
	}()

	var frame *ProgressFrame
	waitFor(t, "a progress frame carrying the engine's row", func() bool {
		for _, candidate := range progressFrames(t, rec) {
			if candidate.Engine != nil && candidate.Engine.RequestID == "eng-123" {
				frame = &candidate
				return true
			}
		}
		return false
	})

	engineProgress := frame.Engine
	if engineProgress.Phase != enginenative.PhaseGenerating {
		t.Fatalf("phase = %q, want %q", engineProgress.Phase, enginenative.PhaseGenerating)
	}
	if engineProgress.GeneratedTokens == nil {
		t.Fatal("the engine reported a generated-token count of zero; it must survive as a reported zero")
	}
	if *engineProgress.GeneratedTokens != 0 {
		t.Fatalf("generated tokens = %d, want the engine's reported zero", *engineProgress.GeneratedTokens)
	}
	// The engine reported no rate, no prefill progress and no queue
	// position: those stay absent rather than becoming zeros.
	if engineProgress.TokensPerSecond != nil {
		t.Fatalf("tokens per second = %v, want it absent — the engine reported none", *engineProgress.TokensPerSecond)
	}
	if engineProgress.PrefillProgress != nil {
		t.Fatalf("prefill progress = %v, want it absent — the engine reported none", *engineProgress.PrefillProgress)
	}
	if engineProgress.QueuePosition != nil {
		t.Fatalf("queue position = %v, want it absent — the engine reported none", *engineProgress.QueuePosition)
	}

	releaseEngine()
	<-done
}

// TestPeerPublishesTheEngineRowsUnderTheOriginsJobId: a request served on
// behalf of a peer publishes the engine's live rows under the origin's job
// id, so a client never joins anything.
func TestPeerPublishesTheEngineRowsUnderTheOriginsJobId(t *testing.T) {
	restorePoll := engineActivityPollInterval
	engineActivityPollInterval = 20 * time.Millisecond
	defer func() { engineActivityPollInterval = restorePoll }()

	em := fakeEngineActivity(t, []enginenative.ActivityRow{{
		RequestID:       "eng-789",
		Phase:           enginenative.PhasePrefilling,
		GeneratedTokens: intPtr(12),
	}})
	restoreURL := engineManagerActivityURL
	engineManagerActivityURL = em.URL + "/v1/engine-activity"
	defer func() { engineManagerActivityURL = restoreURL }()

	release := make(chan struct{})
	var once sync.Once
	releaseEngine := func() { once.Do(func() { close(release) }) }
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(engineRequestIDHeader, "eng-789")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
	}))
	defer engine.Close()
	defer releaseEngine()

	rec := &prRec{}
	p := testProxy(NewDiscovery(), 1235)
	p.codec = NewCodec(rec)
	p.nodeID = "node-peer"

	target, err := url.Parse(engine.URL)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		req.Header.Set(nvpairRequestIDHeader, "job-99")
		p.serveLocalEngine(httptest.NewRecorder(), req, target, req.Header.Get(nvpairRequestIDHeader))
	}()

	var frame *ProgressFrame
	waitFor(t, "a progress frame carrying the engine's row", func() bool {
		for _, candidate := range progressFrames(t, rec) {
			if candidate.Engine != nil && candidate.Engine.RequestID == "eng-789" {
				frame = &candidate
				return true
			}
		}
		return false
	})

	if frame.JobID != "job-99" {
		t.Fatalf("the frame's job id = %q, want the origin's job id 99", frame.JobID)
	}
	if frame.NodeID != "node-peer" {
		t.Fatalf("the frame names node %q, want the serving node", frame.NodeID)
	}
	if frame.Engine.Phase != enginenative.PhasePrefilling {
		t.Fatalf("phase = %q, want %q", frame.Engine.Phase, enginenative.PhasePrefilling)
	}
	if frame.Engine.GeneratedTokens == nil || *frame.Engine.GeneratedTokens != 12 {
		t.Fatalf("generated tokens = %v, want the engine's reported 12", frame.Engine.GeneratedTokens)
	}

	releaseEngine()
	<-done
}

// intPtr is a small helper for the pointers the engine's rows carry.
func intPtr(v int) *int { return &v }

// TestEngineActivityFallsBackToTheEngineAdminAPI: when the engine-manager's
// em surface cannot be reached, the engine's own admin API is the fallback.
// It answers on loopback with the engine's own payload, which PAIR maps onto
// its own type — the API key never leaves this node.
func TestEngineActivityFallsBackToTheEngineAdminAPI(t *testing.T) {
	restorePoll := engineActivityPollInterval
	engineActivityPollInterval = 20 * time.Millisecond
	defer func() { engineActivityPollInterval = restorePoll }()

	// An em surface that is not there: nothing listens on this port.
	restoreURL := engineManagerActivityURL
	engineManagerActivityURL = "http://127.0.0.1:1/v1/engine-activity"
	defer func() { engineManagerActivityURL = restoreURL }()

	release := make(chan struct{})
	var once sync.Once
	releaseEngine := func() { once.Do(func() { close(release) }) }
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin/api/activity" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"active_models":{"m":{"generating":[`+
				`{"request_id":"eng-321","generated_tokens":5,"tokens_per_second":12.5}]}}}`)
			return
		}
		w.Header().Set(engineRequestIDHeader, "eng-321")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
	}))
	defer engine.Close()
	defer releaseEngine()

	rec := &prRec{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "serving-node", engine.URL, "llama"))
	p := NewProxy(NewCodec(rec), disc, 1235)
	p.nodeID = "node-self"
	// The local backend is the engine the fallback reads.
	target, err := url.Parse(engine.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, portStr, err := net.SplitHostPort(target.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	p.setLocalBackend(localBackend{Engine: "omlx", Host: host, Port: port, Healthy: true})

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.handleHTTP(httptest.NewRecorder(),
			httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"llama"}`)))
	}()

	var frame *ProgressFrame
	waitFor(t, "a progress frame carrying the engine's row", func() bool {
		for _, candidate := range progressFrames(t, rec) {
			if candidate.Engine != nil && candidate.Engine.RequestID == "eng-321" {
				frame = &candidate
				return true
			}
		}
		return false
	})

	if frame.Engine.GeneratedTokens == nil || *frame.Engine.GeneratedTokens != 5 {
		t.Fatalf("generated tokens = %v, want the engine's reported 5", frame.Engine.GeneratedTokens)
	}
	if frame.Engine.TokensPerSecond == nil || *frame.Engine.TokensPerSecond != 12.5 {
		t.Fatalf("tokens per second = %v, want the engine's reported 12.5", frame.Engine.TokensPerSecond)
	}
	if frame.Engine.Phase != enginenative.PhaseGenerating {
		t.Fatalf("phase = %q, want %q", frame.Engine.Phase, enginenative.PhaseGenerating)
	}

	releaseEngine()
	<-done
}
