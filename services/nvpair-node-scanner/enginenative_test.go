// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/enginenative"
	"nvpair-shared/noderec"
)

// The engine-native half of the scanner: it reads a node's engine-manager
// /v1/engine-stats over the same transport the model list uses, and folds what
// it reads onto the directory node. These tests pin the transport choice, the
// "a failure is not an error" rule, the fold, the freshness of the reading,
// and the last-good rule a failed fetch follows.

// engineStatsStub is a stub em surface a test drives: the status it answers
// with, the body it answers with, and the observation stamp it reports.
type engineStatsStub struct {
	mu         sync.Mutex
	status     int
	body       map[string]any
	observedAt int64
}

// set swaps what the stub answers with.
func (s *engineStatsStub) set(status int, body map[string]any, observedAt int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
	s.body = body
	s.observedAt = observedAt
}

func (s *engineStatsStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	status, body, observedAt := s.status, s.body, s.observedAt
	s.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	answer := make(map[string]any, len(body)+1)
	for k, v := range body {
		answer[k] = v
	}
	if observedAt != 0 {
		answer["observedAt"] = observedAt
	}
	_ = json.NewEncoder(w).Encode(answer)
}

// startEngineStatsStub stands up a stub engine-manager em surface that answers
// /v1/engine-stats with the given body, and returns the port to dial.
func startEngineStatsStub(t *testing.T, body map[string]any) int {
	t.Helper()
	stub := &engineStatsStub{status: http.StatusOK, body: body}
	return startEngineStatsStubServer(t, stub)
}

// startEngineStatsStubServer stands up a stub em surface driven by stub, and
// returns the port to dial.
func startEngineStatsStubServer(t *testing.T, stub *engineStatsStub) int {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/v1/engine-stats", stub)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return portFromURL(t, srv.URL)
}

// engineNativeBody is a supported engine-native answer, as the em surface
// renders the engine's own counters.
func engineNativeBody() map[string]any {
	return map[string]any{
		"engine":    "omlx",
		"supported": true,
		"totals": map[string]any{
			"requests": 2, "promptTokens": 120, "completionTokens": 34,
			"cachedTokens": 90, "cacheEfficiency": 75,
		},
		"rates": map[string]any{"prefillTokensPerSecond": 100, "generationTokensPerSecond": 50},
		"models": []any{
			map[string]any{"model": "mlx-community/Llama-3.2-3B-Instruct-4bit", "loaded": true},
		},
	}
}

// newEngineNativeTestDaemon is a daemon with just what the engine-native path
// needs: a directory, a registry (so the self shortcut is off for a peer),
// and the loopback client the models transport uses.
func newEngineNativeTestDaemon() *daemon {
	return &daemon{
		reg:        newRegistry("self-node-uuid", "", selfAddrs("127.0.0.1")),
		dir:        newDirectory(),
		modelsHTTP: &http.Client{Timeout: modelsFetchTimeout},
	}
}

// notifyCapture collects the frames the daemon emits, so a test can read the
// node a notification carried.
type notifyCapture struct {
	mu     sync.Mutex
	frames []byte
}

func (c *notifyCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frames = append(c.frames, p...)
	return len(p), nil
}

func (c *notifyCapture) Read(p []byte) (int, error) { return 0, io.EOF }

// nodes returns the nodes the captured notifications carried.
func (c *notifyCapture) nodes(t *testing.T) []noderec.DirectoryNode {
	t.Helper()
	c.mu.Lock()
	raw := append([]byte(nil), c.frames...)
	c.mu.Unlock()
	var out []noderec.DirectoryNode
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var msg struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			t.Fatalf("emitted frame is not JSON: %v (%s)", err, line)
		}
		var ev noderec.NodeEvent
		if err := json.Unmarshal(msg.Params, &ev); err != nil {
			t.Fatalf("emitted params are not a node event: %v", err)
		}
		out = append(out, ev.Node)
	}
	return out
}

// TestEngineNativeFetchOverLoopbackForThisNode pins the transport for this
// node's own engine-manager: the em surface serves loopback in plaintext, in
// every membership state, so a standalone machine still sees its own engine's
// counters. The stub speaks plaintext only, so a fetch that took the pinned
// mTLS path could not have answered.
func TestEngineNativeFetchOverLoopbackForThisNode(t *testing.T) {
	d := &daemon{modelsHTTP: &http.Client{Timeout: modelsFetchTimeout}}
	port := startEngineStatsStub(t, engineNativeBody())

	view, ok := d.fetchEngineStats(context.Background(), "127.0.0.1", port, "")
	if !ok {
		t.Fatal("this node's own engine-native view was not read over loopback")
	}
	if view.Engine != enginenative.EngineOmlx || !view.Supported {
		t.Fatalf("engine-native view = %+v, want the omlx view", view)
	}
	if view.Totals.Requests != 2 || view.Totals.PromptTokens != 120 {
		t.Errorf("engine-native totals = %+v, want the engine's own counters", view.Totals)
	}
	if view.Rates.GenerationTokensPerSecond != 50 {
		t.Errorf("engine-native rates = %+v, want the engine's own rates", view.Rates)
	}
	if len(view.Models) != 1 || !view.Models[0].Loaded {
		t.Errorf("engine-native models = %+v, want the engine's own list", view.Models)
	}
	if view.ObservedAt == 0 {
		t.Error("the engine-native view carries no observation stamp")
	}
}

// TestEngineNativeFetchOverPinnedMTLSForAPeer pins the transport for a peer:
// the engine's counters are cluster data, so a peer is read over cluster mTLS
// keyed on the principal it advertises. The stub speaks TLS only, so a
// plaintext fetch could not have answered — and a peer this node holds no pin
// for is not fetched at all, which is a normal state rather than an error.
func TestEngineNativeFetchOverPinnedMTLSForAPeer(t *testing.T) {
	selfDir := t.TempDir()
	peerDir := t.TempDir()
	clustertrusttest.WriteKeypair(t, selfDir, "uuid-self")
	clustertrusttest.WriteKeypair(t, peerDir, "uuid-peer")
	clustertrusttest.WriteAdmission(t, selfDir, "cluster-1", 1)
	clustertrusttest.WriteAdmission(t, peerDir, "cluster-1", 1)
	writePinFromCert(t, selfDir, "uuid-peer", filepath.Join(peerDir, "node.crt"))
	writePinFromCert(t, peerDir, "uuid-self", filepath.Join(selfDir, "node.crt"))

	selfMesh := clustertrust.Open(selfDir)
	peerMesh := clustertrust.Open(peerDir)
	d := &daemon{
		mesh:          selfMesh,
		modelsHTTP:    &http.Client{Timeout: modelsFetchTimeout},
		modelsClients: clustertrust.NewPeerClientPool(selfMesh, modelsFetchTimeout),
	}
	defer d.modelsClients.CloseIdle()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/engine-stats", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := peerMesh.VerifyClientPin(r); !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(engineNativeBody())
	})
	ts := httptest.NewUnstartedServer(mux)
	ts.TLS = peerMesh.ServerTLSConfig()
	ts.StartTLS()
	defer ts.Close()
	port := portFromURL(t, ts.URL)

	// "localhost" is not a loopback IP literal, so modelsClient takes the
	// pinned mTLS path for it, exactly as it does for the model list.
	view, ok := d.fetchEngineStats(context.Background(), "localhost", port, "uuid-peer")
	if !ok {
		t.Fatal("a pinned peer's engine-native view was not read over mTLS")
	}
	if view.Totals.Requests != 2 {
		t.Errorf("engine-native totals = %+v, want the engine's own counters", view.Totals)
	}

	// A peer this node holds no pin for is not read at all: no view, and no
	// error. The engine's counters are cluster data, and this node has no
	// claim to them.
	view, ok = d.fetchEngineStats(context.Background(), "localhost", port, "uuid-stranger")
	if ok || view != nil {
		t.Fatalf("an unpinned peer produced an engine-native view: %+v (ok=%v)", view, ok)
	}
}

// TestEngineNativeViewLandsOnTheDirectoryNode covers the fold: a fetched view
// lands on the directory node, a re-read of the same observation reports no
// change, and an engine with no admin API leaves the node with no view at all.
func TestEngineNativeViewLandsOnTheDirectoryNode(t *testing.T) {
	d := newEngineNativeTestDaemon()
	stub := &engineStatsStub{status: http.StatusOK, body: engineNativeBody(), observedAt: 1000}
	port := startEngineStatsStubServer(t, stub)
	seedEMNode(d, "peer-A", "127.0.0.1", port)

	if !d.refreshNodeEngineNative(context.Background(), "peer-A", "127.0.0.1", []string{"127.0.0.1"}, port, "") {
		t.Fatal("the engine-native view was not applied to the directory node")
	}
	got, _ := d.dir.get("peer-A")
	if got.EngineNative == nil {
		t.Fatal("the directory node carries no engine-native view")
	}
	if got.EngineNative.Totals.Requests != 2 {
		t.Errorf("engine-native totals = %+v, want the engine's own counters", got.EngineNative.Totals)
	}
	// The model list beside it is untouched: the two are separate reads.
	if got.Models != nil {
		t.Errorf("the engine-native fetch disturbed the model list: %v", got.Models)
	}

	// The same observation again is not a change: the same numbers, read at
	// the same stamp.
	if d.refreshNodeEngineNative(context.Background(), "peer-A", "127.0.0.1", []string{"127.0.0.1"}, port, "") {
		t.Fatal("an unchanged engine-native view reported a change")
	}

	// An engine with no admin API reports "unsupported", which is not a view:
	// the node keeps the view it had and gains no new one.
	unsupported := startEngineStatsStub(t, map[string]any{"engine": "omlx", "supported": false})
	seedEMNode(d, "peer-B", "127.0.0.1", unsupported)
	if d.refreshNodeEngineNative(context.Background(), "peer-B", "127.0.0.1", []string{"127.0.0.1"}, unsupported, "") {
		t.Fatal("an unsupported engine produced an engine-native view")
	}
	got, _ = d.dir.get("peer-B")
	if got.EngineNative != nil {
		t.Fatalf("a node with no admin API carries an engine-native view: %+v", got.EngineNative)
	}
}

// TestEngineNativeObservationAdvancesOnASweep pins the freshness of the
// reading: every sweep is a new observation, so the stamp advances even when
// the engine's counters do not move. That is what lets a reader compute
// staleness as now - observedAt, and what tells a live but idle engine apart
// from one that died an hour ago.
func TestEngineNativeObservationAdvancesOnASweep(t *testing.T) {
	d := newEngineNativeTestDaemon()
	stub := &engineStatsStub{status: http.StatusOK, body: engineNativeBody(), observedAt: 1000}
	port := startEngineStatsStubServer(t, stub)
	seedEMNode(d, "peer-A", "127.0.0.1", port)

	if !d.refreshNodeEngineNative(context.Background(), "peer-A", "127.0.0.1", []string{"127.0.0.1"}, port, "") {
		t.Fatal("the first reading was not applied to the directory node")
	}
	got, _ := d.dir.get("peer-A")
	if got.EngineNative == nil || got.EngineNative.ObservedAt != 1000 {
		t.Fatalf("engine-native view = %+v, want the reading's own stamp", got.EngineNative)
	}

	// The counters did not move, but the observation is newer: that is a
	// change worth reporting, because the stamp is the freshness signal.
	stub.set(http.StatusOK, engineNativeBody(), 2000)
	if !d.refreshNodeEngineNative(context.Background(), "peer-A", "127.0.0.1", []string{"127.0.0.1"}, port, "") {
		t.Fatal("a newer observation was not reported as a change")
	}
	got, _ = d.dir.get("peer-A")
	if got.EngineNative == nil || got.EngineNative.ObservedAt != 2000 {
		t.Fatalf("engine-native view = %+v, want the newer stamp", got.EngineNative)
	}
	if got.EngineNative.Totals.Requests != 2 {
		t.Errorf("engine-native totals = %+v, want the engine's own counters", got.EngineNative.Totals)
	}

	// The very same reading again is not a change: nothing about it moved.
	if d.refreshNodeEngineNative(context.Background(), "peer-A", "127.0.0.1", []string{"127.0.0.1"}, port, "") {
		t.Fatal("the same reading reported a change")
	}
}

// TestEngineNativeModelOrderIsNotAChange pins that the engine's model list is
// compared as a set: the engine lists its active models in no particular
// order, so the same models in a different order are the same reading and
// must not be announced as a change.
func TestEngineNativeModelOrderIsNotAChange(t *testing.T) {
	d := newEngineNativeTestDaemon()
	first := engineNativeBody()
	first["models"] = []any{
		map[string]any{"model": "mlx-community/Llama-3.2-3B-Instruct-4bit", "loaded": true},
		map[string]any{"model": "mlx-community/Qwen3-4B-4bit", "loaded": true},
	}
	stub := &engineStatsStub{status: http.StatusOK, body: first, observedAt: 1000}
	port := startEngineStatsStubServer(t, stub)
	seedEMNode(d, "peer-A", "127.0.0.1", port)

	if !d.refreshNodeEngineNative(context.Background(), "peer-A", "127.0.0.1", []string{"127.0.0.1"}, port, "") {
		t.Fatal("the first reading was not applied to the directory node")
	}

	reordered := engineNativeBody()
	reordered["models"] = []any{
		map[string]any{"model": "mlx-community/Qwen3-4B-4bit", "loaded": true},
		map[string]any{"model": "mlx-community/Llama-3.2-3B-Instruct-4bit", "loaded": true},
	}
	stub.set(http.StatusOK, reordered, 1000)
	if d.refreshNodeEngineNative(context.Background(), "peer-A", "127.0.0.1", []string{"127.0.0.1"}, port, "") {
		t.Fatal("a reordered model list reported a change")
	}
}

// TestEngineNativeViewSurvivesARecordReplacement covers the flap: the browse
// path replaces the stored record wholesale, and the record it builds carries
// no engine-native view (the mDNS TXT has none). The stored view must survive
// the replacement — otherwise every advertisement deletes the view and the
// next sweep brings it back, and the client watches the field appear and
// disappear.
func TestEngineNativeViewSurvivesARecordReplacement(t *testing.T) {
	d := newEngineNativeTestDaemon()
	capture := &notifyCapture{}
	d.codec = NewCodec(capture)
	stub := &engineStatsStub{status: http.StatusOK, body: engineNativeBody(), observedAt: 1000}
	port := startEngineStatsStubServer(t, stub)
	seedEMNode(d, "peer-A", "127.0.0.1", port)

	if !d.refreshNodeEngineNative(context.Background(), "peer-A", "127.0.0.1", []string{"127.0.0.1"}, port, "") {
		t.Fatal("the engine-native view was not applied to the directory node")
	}

	// The peer advertises again: the record is rebuilt from the mDNS TXT,
	// which carries no engine-native view.
	d.onBrowse(DiscoveryEvent{Type: "updated", Node: RawNode{
		ID:  "peer-A",
		TXT: []string{"v=1", "uuid=peer-A", "ip=127.0.0.1"},
	}})

	stored, ok := d.dir.get("peer-A")
	if !ok {
		t.Fatal("the advertisement dropped the node from the directory")
	}
	if stored.EngineNative == nil {
		t.Fatal("the record replacement deleted the engine-native view")
	}
	if stored.EngineNative.Totals.Requests != 2 {
		t.Errorf("engine-native totals = %+v, want the engine's own counters", stored.EngineNative.Totals)
	}

	// The announced record carries the view too: a client that reads the
	// event must not see the field disappear.
	nodes := capture.nodes(t)
	if len(nodes) == 0 {
		t.Fatal("the advertisement announced nothing")
	}
	last := nodes[len(nodes)-1]
	if last.EngineNative == nil {
		t.Fatal("the announced record dropped the engine-native view")
	}
	if last.EngineNative.ObservedAt != 1000 {
		t.Errorf("announced observation stamp = %d, want the stored reading's stamp", last.EngineNative.ObservedAt)
	}
}

// TestEngineNativeKeepsTheLastGoodViewWhenTheFetchFails pins the last-good
// rule: a node whose fetch fails keeps the view it had — the rule the model
// list follows too — while a node that never produced a view has none.
func TestEngineNativeKeepsTheLastGoodViewWhenTheFetchFails(t *testing.T) {
	d := newEngineNativeTestDaemon()
	stub := &engineStatsStub{status: http.StatusOK, body: engineNativeBody(), observedAt: 1000}
	port := startEngineStatsStubServer(t, stub)
	seedEMNode(d, "peer-A", "127.0.0.1", port)

	if !d.refreshNodeEngineNative(context.Background(), "peer-A", "127.0.0.1", []string{"127.0.0.1"}, port, "") {
		t.Fatal("the engine-native view was not applied to the directory node")
	}
	before, _ := d.dir.get("peer-A")

	// The engine stops answering.
	stub.set(http.StatusInternalServerError, nil, 0)
	if d.refreshNodeEngineNative(context.Background(), "peer-A", "127.0.0.1", []string{"127.0.0.1"}, port, "") {
		t.Fatal("a failed fetch reported a change")
	}

	after, _ := d.dir.get("peer-A")
	if after.EngineNative == nil {
		t.Fatal("a failed fetch deleted the engine-native view")
	}
	if !enginenative.SameNumbers(before.EngineNative, after.EngineNative) {
		t.Errorf("engine-native view = %+v, want the last good one %+v", after.EngineNative, before.EngineNative)
	}
	if after.EngineNative.ObservedAt != before.EngineNative.ObservedAt {
		t.Errorf("observation stamp = %d, want the last good stamp %d",
			after.EngineNative.ObservedAt, before.EngineNative.ObservedAt)
	}
}

// TestEngineNativeFailingFetchReportsNoContentChange pins that a fetch which
// failed reports no change and announces nothing: the reading did not move,
// so there is nothing to say about it.
func TestEngineNativeFailingFetchReportsNoContentChange(t *testing.T) {
	d := newEngineNativeTestDaemon()
	capture := &notifyCapture{}
	d.codec = NewCodec(capture)
	stub := &engineStatsStub{status: http.StatusOK, body: engineNativeBody(), observedAt: 1000}
	port := startEngineStatsStubServer(t, stub)
	seedEMNode(d, "peer-A", "127.0.0.1", port)

	if !d.refreshNodeEngineNative(context.Background(), "peer-A", "127.0.0.1", []string{"127.0.0.1"}, port, "") {
		t.Fatal("the engine-native view was not applied to the directory node")
	}
	announced := len(capture.nodes(t))

	// The engine stops answering.
	stub.set(http.StatusServiceUnavailable, nil, 0)
	if d.refreshNodeEngineNative(context.Background(), "peer-A", "127.0.0.1", []string{"127.0.0.1"}, port, "") {
		t.Fatal("a failed fetch reported a content change")
	}
	if got := len(capture.nodes(t)); got != announced {
		t.Fatalf("a failed fetch announced %d extra records, want none", got-announced)
	}
}

// deadlineRecorder records the deadline the outgoing request carries.
type deadlineRecorder struct {
	base      http.RoundTripper
	deadlines chan time.Time
}

func (t *deadlineRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	deadline, ok := req.Context().Deadline()
	if !ok {
		deadline = time.Time{}
	}
	t.deadlines <- deadline
	return t.base.RoundTrip(req)
}

// TestEngineNativeFetchIsBounded pins the bound on the scanner's own call:
// the em surface is bounded, but the scanner's read is bounded too, so a peer
// that accepts the connection and then says nothing cannot hold the sweep
// open. The bound is the deadline the outgoing request carries.
func TestEngineNativeFetchIsBounded(t *testing.T) {
	port := startEngineStatsStub(t, engineNativeBody())
	recorder := &deadlineRecorder{base: http.DefaultTransport, deadlines: make(chan time.Time, 1)}
	d := &daemon{modelsHTTP: &http.Client{Timeout: engineNativeFetchTimeout, Transport: recorder}}

	if _, ok := d.fetchEngineStats(context.Background(), "127.0.0.1", port, ""); !ok {
		t.Fatal("the engine-native fetch did not answer")
	}
	deadline := <-recorder.deadlines
	if deadline.IsZero() {
		t.Fatal("the engine-native fetch carried no deadline; a silent peer could hold it open")
	}
	if until := time.Until(deadline); until <= 0 || until > engineNativeFetchTimeout {
		t.Fatalf("the engine-native fetch deadline is %s away, want at most %s", until, engineNativeFetchTimeout)
	}
}

// TestEngineNativeFetchFollowsTheDaemonContext pins that the read rides the
// daemon's own context: a shutdown cancels an in-flight read instead of
// leaving it to run out its deadline.
func TestEngineNativeFetchFollowsTheDaemonContext(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/engine-stats", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	port := portFromURL(t, srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		cancel()
	}()

	d := &daemon{modelsHTTP: &http.Client{Timeout: engineNativeFetchTimeout}}
	if _, ok := d.fetchEngineStats(ctx, "127.0.0.1", port, ""); ok {
		t.Fatal("a cancelled read still produced a view")
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight read was not cancelled with the daemon's context")
	}
}

// TestEngineNativeNon200DrainsTheAnswer pins the connection hygiene: an
// answer that is not a 200 is drained before its body is closed, so the
// pooled transport can reuse the connection instead of opening a new one for
// every failed read.
func TestEngineNativeNon200DrainsTheAnswer(t *testing.T) {
	var mu sync.Mutex
	opened := 0
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("the engine said no"))
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			mu.Lock()
			opened++
			mu.Unlock()
		}
	}
	srv.Start()
	defer srv.Close()
	port := portFromURL(t, srv.URL)

	d := &daemon{modelsHTTP: &http.Client{Timeout: engineNativeFetchTimeout}}
	for i := 0; i < 3; i++ {
		if _, ok := d.fetchEngineStats(context.Background(), "127.0.0.1", port, ""); ok {
			t.Fatal("a non-200 answer produced a view")
		}
	}
	mu.Lock()
	got := opened
	mu.Unlock()
	if got != 1 {
		t.Fatalf("three failed reads opened %d connections, want 1: the answer was not drained", got)
	}
}
