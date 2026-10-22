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
	p.nodeID = "node-origin"

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

// TestForwardedPeerRequestCarriesTheOrigin: a request forwarded to a cluster
// peer carries the origin node's id alongside the job id. PAIR's job id is a
// per-proxy counter, so the origin is what tells two origins' job "1" apart —
// the serving peer publishes the engine's live rows under that id.
func TestForwardedPeerRequestCarriesTheOrigin(t *testing.T) {
	p := testProxy(NewDiscovery(), 1235)
	p.nodeID = "node-origin"

	peer := candidate{
		id:       "peer-a",
		url:      &url.URL{Scheme: "https", Host: "192.0.2.10:1234"},
		peerUUID: "principal-peer",
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	p.forwardDirector(peer, "42")(req)
	if got := req.Header.Get(nvpairOriginNodeHeader); got != "node-origin" {
		t.Fatalf("%s = %q, want the origin's node id", nvpairOriginNodeHeader, got)
	}

	// A request that is not forwarded to a peer carries neither header: the
	// peer is the only node that needs them.
	local := candidate{
		id:  "local-node",
		url: &url.URL{Scheme: "http", Host: "127.0.0.1:8000"},
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	p.forwardDirector(local, "42")(req)
	if got := req.Header.Get(nvpairOriginNodeHeader); got != "" {
		t.Fatalf("%s = %q, want it absent on a request that is not forwarded to a peer", nvpairOriginNodeHeader, got)
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
		p.serveLocalEngine(httptest.NewRecorder(), req, target,
			req.Header.Get(nvpairRequestIDHeader),
			req.Header.Get(nvpairOriginNodeHeader),
			req.Header.Get(nvpairRunIDHeader))
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
			if candidate.EngineProgress != nil && candidate.EngineProgress.RequestID == "eng-123" {
				frame = &candidate
				return true
			}
		}
		return false
	})

	engineProgress := frame.EngineProgress
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
		req.Header.Set(nvpairOriginNodeHeader, "origin-a")
		p.serveLocalEngine(httptest.NewRecorder(), req, target,
			req.Header.Get(nvpairRequestIDHeader),
			req.Header.Get(nvpairOriginNodeHeader),
			req.Header.Get(nvpairRunIDHeader))
	}()

	var frame *ProgressFrame
	waitFor(t, "a progress frame carrying the engine's row", func() bool {
		for _, candidate := range progressFrames(t, rec) {
			if candidate.EngineProgress != nil && candidate.EngineProgress.RequestID == "eng-789" {
				frame = &candidate
				return true
			}
		}
		return false
	})

	if frame.JobID != "job-99" {
		t.Fatalf("the frame's job id = %q, want the origin's job id 99", frame.JobID)
	}
	if frame.OriginatedFrom != "origin-a" {
		t.Fatalf("the frame's origin = %q, want the origin that forwarded the request", frame.OriginatedFrom)
	}
	if frame.NodeID != "node-peer" {
		t.Fatalf("the frame names node %q, want the serving node", frame.NodeID)
	}
	if frame.EngineProgress.Phase != enginenative.PhasePrefilling {
		t.Fatalf("phase = %q, want %q", frame.EngineProgress.Phase, enginenative.PhasePrefilling)
	}
	if frame.EngineProgress.GeneratedTokens == nil || *frame.EngineProgress.GeneratedTokens != 12 {
		t.Fatalf("generated tokens = %v, want the engine's reported 12", frame.EngineProgress.GeneratedTokens)
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
	// The local engine is the one the fallback reads.
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
			if candidate.EngineProgress != nil && candidate.EngineProgress.RequestID == "eng-321" {
				frame = &candidate
				return true
			}
		}
		return false
	})

	if frame.EngineProgress.GeneratedTokens == nil || *frame.EngineProgress.GeneratedTokens != 5 {
		t.Fatalf("generated tokens = %v, want the engine's reported 5", frame.EngineProgress.GeneratedTokens)
	}
	if frame.EngineProgress.TokensPerSecond == nil || *frame.EngineProgress.TokensPerSecond != 12.5 {
		t.Fatalf("tokens per second = %v, want the engine's reported 12.5", frame.EngineProgress.TokensPerSecond)
	}
	if frame.EngineProgress.Phase != enginenative.PhaseGenerating {
		t.Fatalf("phase = %q, want %q", frame.EngineProgress.Phase, enginenative.PhaseGenerating)
	}

	releaseEngine()
	<-done
}

// TestForwardedPeerRequestCarriesTheRunId: a request forwarded to a cluster
// peer carries the origin proxy's run nonce alongside the job id. A proxy
// that restarts counts its jobs from 1 again, and the run nonce says which
// run the job id belongs to, so the serving peer never takes the job "1" of
// the new run for the job "1" of the old one.
func TestForwardedPeerRequestCarriesTheRunId(t *testing.T) {
	p := testProxy(NewDiscovery(), 1235)
	p.nodeID = "node-origin"

	peer := candidate{
		id:       "peer-a",
		url:      &url.URL{Scheme: "https", Host: "192.0.2.10:1234"},
		peerUUID: "principal-peer",
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	p.forwardDirector(peer, "42")(req)
	if got := req.Header.Get(nvpairRunIDHeader); got != p.runID {
		t.Fatalf("%s = %q, want the proxy's run nonce %q", nvpairRunIDHeader, got, p.runID)
	}

	// A request that is not forwarded to a peer carries no run nonce: the
	// peer is the only node that needs it.
	local := candidate{
		id:  "local-node",
		url: &url.URL{Scheme: "http", Host: "127.0.0.1:8000"},
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	p.forwardDirector(local, "42")(req)
	if got := req.Header.Get(nvpairRunIDHeader); got != "" {
		t.Fatalf("%s = %q, want it absent on a request that is not forwarded to a peer", nvpairRunIDHeader, got)
	}
}

// TestTheLocalPollerDoesNotBindAPeerServedJob: a job served by a cluster
// peer is served by the PEER's engine, and the peer pairs the engine's
// request id with the job id itself. The local poller must not bind it: it
// would hunt a peer engine's request id in the local engine's rows.
func TestTheLocalPollerDoesNotBindAPeerServedJob(t *testing.T) {
	peer := candidate{
		id:       "peer-a",
		url:      &url.URL{Scheme: "https", Host: "192.0.2.10:1234"},
		peerUUID: "principal-peer",
	}
	if bindsLocalEngineJob(peer) {
		t.Fatal("a peer-served job is bound in the local activity poller")
	}

	local := candidate{
		id:  "local-node",
		url: &url.URL{Scheme: "http", Host: "127.0.0.1:8000"},
	}
	if !bindsLocalEngineJob(local) {
		t.Fatal("a job the local engine serves is not bound in the local activity poller")
	}
}

// TestTheTwoPublishersShareOneLimiter: a job's live view is published by
// two publishers — the PAIR-derived facts and the engine's own reading —
// and they share the job's one limiter. So a job leaves at most one frame
// per window, whichever publisher has something to say, and the documented
// bound ("at most one progress frame per job every two seconds") holds.
//
// A frame the limiter holds back loses nothing: the broker merges the two
// partial views field by field, so the numbers the other publisher measured
// stay in the merged view.
func TestTheTwoPublishersShareOneLimiter(t *testing.T) {
	restoreInterval := progressInterval
	progressInterval = time.Hour
	defer func() { progressInterval = restoreInterval }()
	restorePoll := engineActivityPollInterval
	engineActivityPollInterval = 20 * time.Millisecond
	defer func() { engineActivityPollInterval = restorePoll }()

	em := fakeEngineActivity(t, []enginenative.ActivityRow{{
		RequestID: "eng-123",
		Phase:     enginenative.PhaseGenerating,
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

	waitFor(t, "the first live frame of the job", func() bool {
		return len(progressFrames(t, rec)) > 0
	})
	// The engine's own publisher polls every 20 ms. None of its polls may
	// leave a second frame while the window is open.
	time.Sleep(200 * time.Millisecond)

	frames := progressFrames(t, rec)
	if len(frames) != 1 {
		t.Fatalf("progress frames = %d, want 1: the two publishers share the job's one limiter", len(frames))
	}

	releaseEngine()
	<-done
}

// TestTheFramesCarryTheEnginesLiveNumbers: a job's live frames carry the
// engine's own live numbers.
//
// A job's live view has two halves — the facts PAIR measured and the
// engine's own reading — and two publishers, one for each half. They share
// the job's one window, so exactly one of them leaves a frame in it, and the
// publisher that is driven by the response's chunks (the PAIR-derived one)
// wins nearly every window. A frame that carried only its own half would
// therefore leave the other half to a publisher that never wins: the
// engine's numbers would never reach a client.
//
// The test reproduces that: the engine's publisher polls slower than the
// window, so it never wins one, and every frame is the request's own. Those
// frames must still carry the engine's reading.
func TestTheFramesCarryTheEnginesLiveNumbers(t *testing.T) {
	restoreInterval := progressInterval
	progressInterval = 50 * time.Millisecond
	defer func() { progressInterval = restoreInterval }()
	restorePoll := engineActivityPollInterval
	// The engine's publisher polls slower than the job's window, so it
	// never wins one: every frame is the request's own publisher's.
	engineActivityPollInterval = 400 * time.Millisecond
	defer func() { engineActivityPollInterval = restorePoll }()

	em := fakeEngineActivity(t, []enginenative.ActivityRow{{
		RequestID:       "eng-123",
		Phase:           enginenative.PhaseGenerating,
		GeneratedTokens: intPtr(42),
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
		flusher, _ := w.(http.Flusher)
		// The engine keeps sending, so the request's own publisher is
		// driven throughout the job's life.
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-release:
				return
			case <-ticker.C:
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
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

	var frame ProgressFrame
	waitFor(t, "a frame carrying the engine's live numbers", func() bool {
		for _, candidate := range progressFrames(t, rec) {
			if candidate.EngineProgress == nil {
				continue
			}
			if candidate.EngineProgress.RequestID != "eng-123" {
				continue
			}
			frame = candidate
			return true
		}
		return false
	})

	if frame.EngineProgress.Phase != enginenative.PhaseGenerating {
		t.Fatalf("phase = %q, want %q", frame.EngineProgress.Phase, enginenative.PhaseGenerating)
	}
	if frame.EngineProgress.GeneratedTokens == nil || *frame.EngineProgress.GeneratedTokens != 42 {
		t.Fatalf("generated tokens = %v, want the engine's reported 42", frame.EngineProgress.GeneratedTokens)
	}
	// The frame that carries the engine's numbers carries PAIR's own facts
	// as well: both halves of the view travel together, so a frame never
	// leaves one of them behind.
	if frame.Bytes == nil || *frame.Bytes == 0 {
		t.Fatalf("bytes = %v, want the bytes PAIR measured", frame.Bytes)
	}
	if frame.ElapsedMs == nil {
		t.Fatal("elapsed time is absent, want the elapsed time PAIR measured")
	}

	releaseEngine()
	<-done
}
