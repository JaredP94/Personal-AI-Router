// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/enginenative"
)

// The engine-native half of the scanner: it reads a node's engine-manager
// /v1/engine-stats over the same transport the model list uses, and folds what
// it reads onto the directory node. These tests pin the transport choice, the
// "a failure is not an error" rule, and the fold.

// startEngineStatsStub stands up a stub engine-manager em surface that answers
// /v1/engine-stats with the given body, and returns the port to dial.
func startEngineStatsStub(t *testing.T, body map[string]any) int {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/engine-stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
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

// TestEngineNativeFetchOverLoopbackForThisNode pins the transport for this
// node's own engine-manager: the em surface serves loopback in plaintext, in
// every membership state, so a standalone machine still sees its own engine's
// counters. The stub speaks plaintext only, so a fetch that took the pinned
// mTLS path could not have answered.
func TestEngineNativeFetchOverLoopbackForThisNode(t *testing.T) {
	d := &daemon{modelsHTTP: &http.Client{Timeout: modelsFetchTimeout}}
	port := startEngineStatsStub(t, engineNativeBody())

	view, ok := d.fetchEngineStats("127.0.0.1", port, "")
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
	view, ok := d.fetchEngineStats("localhost", port, "uuid-peer")
	if !ok {
		t.Fatal("a pinned peer's engine-native view was not read over mTLS")
	}
	if view.Totals.Requests != 2 {
		t.Errorf("engine-native totals = %+v, want the engine's own counters", view.Totals)
	}

	// A peer this node holds no pin for is not read at all: no view, and no
	// error. The engine's counters are cluster data, and this node has no
	// claim to them.
	view, ok = d.fetchEngineStats("localhost", port, "uuid-stranger")
	if ok || view != nil {
		t.Fatalf("an unpinned peer produced an engine-native view: %+v (ok=%v)", view, ok)
	}
}

// TestEngineNativeViewLandsOnTheDirectoryNode covers the fold: a fetched view
// lands on the directory node, a repeated fetch of the same numbers reports no
// change (the observation stamp moves, the numbers do not), and an engine with
// no admin API leaves the node with no view at all.
func TestEngineNativeViewLandsOnTheDirectoryNode(t *testing.T) {
	d := &daemon{
		reg:        newRegistry("self-node-uuid", "", selfAddrs("127.0.0.1")),
		dir:        newDirectory(),
		modelsHTTP: &http.Client{Timeout: modelsFetchTimeout},
	}
	port := startEngineStatsStub(t, engineNativeBody())
	seedEMNode(d, "peer-A", "127.0.0.1", port)

	if !d.refreshNodeEngineNative("peer-A", "127.0.0.1", []string{"127.0.0.1"}, port, "") {
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

	// The same numbers again are not a change: the observation stamp moves on
	// every read, but the engine's counters did not.
	if d.refreshNodeEngineNative("peer-A", "127.0.0.1", []string{"127.0.0.1"}, port, "") {
		t.Fatal("an unchanged engine-native view reported a change")
	}

	// An engine with no admin API reports "unsupported", which is not a view:
	// the node keeps the view it had and gains no new one.
	unsupported := startEngineStatsStub(t, map[string]any{"engine": "omlx", "supported": false})
	seedEMNode(d, "peer-B", "127.0.0.1", unsupported)
	if d.refreshNodeEngineNative("peer-B", "127.0.0.1", []string{"127.0.0.1"}, unsupported, "") {
		t.Fatal("an unsupported engine produced an engine-native view")
	}
	got, _ = d.dir.get("peer-B")
	if got.EngineNative != nil {
		t.Fatalf("a node with no admin API carries an engine-native view: %+v", got.EngineNative)
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

	if _, ok := d.fetchEngineStats("127.0.0.1", port, ""); !ok {
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
