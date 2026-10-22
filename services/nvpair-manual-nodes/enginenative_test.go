// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/enginenative"
)

// The engine-native half of a manual node: the engine's own report of itself,
// read over the same pinned mTLS path the model list beside it is read over.
//
// A manual node is a node this host reaches by address rather than by mDNS,
// so the node-scanner — which fetches the engine's view for the nodes it
// discovers — never sees it. These tests pin that the manual-node prober
// carries the same view, so a peer and a discovered node present the engine's
// counters identically.

// engineStatsStub is a stub em surface a test drives: the answer it gives for
// /v1/engine-stats, and how the surface was reached.
type engineStatsStub struct {
	mu      sync.Mutex
	body    map[string]any
	sawCert bool
	sawPlan bool
}

// set swaps the answer the stub gives.
func (s *engineStatsStub) set(body map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.body = body
}

// sawClientCert reports whether the stub was reached over mTLS with a client
// certificate.
func (s *engineStatsStub) sawClientCert() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sawCert
}

// sawPlaintext reports whether the stub was reached without TLS.
func (s *engineStatsStub) sawPlaintext() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sawPlan
}

// engineNativeBody is a supported engine-native answer, as the em surface
// renders the engine's own counters.
func engineNativeBody() map[string]any {
	return map[string]any{
		"engine":    "omlx",
		"supported": true,
		"totals": map[string]any{
			"requests": 2, "promptTokens": 120, "completionTokens": 34,
		},
		"rates": map[string]any{"prefillTokensPerSecond": 100, "generationTokensPerSecond": 50},
		"models": []any{
			map[string]any{"model": "Jundot--Qwen3.8-Flash-Next-oQ4e-mtp", "loaded": true},
		},
		"observedAt": 1792663511584,
	}
}

// writePinFromCert pins the certificate at certPath into clusterDir's trusted
// set under peerUUID, the way nvpair-cluster-manager does after a pairing.
func writePinFromCert(t *testing.T, clusterDir, peerUUID, certPath string) {
	t.Helper()
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	trusted := filepath.Join(clusterDir, "trusted")
	if err := os.MkdirAll(trusted, 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]string{"nodeUuid": peerUUID, "certPem": string(certPEM)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trusted, peerUUID+".json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// newClusterSelfDir creates this node's side of a cluster: its own keypair and
// its live admission. The pin for the peer is written separately, once the
// peer's certificate exists.
func newClusterSelfDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	clustertrusttest.WriteKeypair(t, dir, "uuid-self")
	clustertrusttest.WriteAdmission(t, dir, "cluster-1", 1)
	return dir
}

// startPeerEngineManager stands up a peer's engine-manager surface: the model
// list and the engine's own view, both behind the peer's pin gate, served over
// cluster mTLS. It pins the peer's certificate into selfDir (so this node
// holds a pin for the peer) and this node's certificate into the peer's own
// cluster dir (so the peer holds a pin for us). It returns the host and port
// to dial and the stub that records how the surface was reached.
func startPeerEngineManager(t *testing.T, selfDir string, body map[string]any) (string, int, *engineStatsStub) {
	t.Helper()
	peerDir := t.TempDir()
	clustertrusttest.WriteKeypair(t, peerDir, "uuid-peer")
	clustertrusttest.WriteAdmission(t, peerDir, "cluster-1", 1)
	writePinFromCert(t, selfDir, "uuid-peer", filepath.Join(peerDir, "node.crt"))
	writePinFromCert(t, peerDir, "uuid-self", filepath.Join(selfDir, "node.crt"))

	peerMesh := clustertrust.Open(peerDir)
	stub := &engineStatsStub{body: body}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := peerMesh.VerifyClientPin(r); !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []string{"a-model"}})
	})
	mux.HandleFunc("/v1/engine-stats", func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		stub.sawCert = r.TLS != nil && len(r.TLS.PeerCertificates) > 0
		stub.sawPlan = r.TLS == nil
		answer := stub.body
		stub.mu.Unlock()
		if _, ok := peerMesh.VerifyClientPin(r); !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(answer)
	})
	ts := httptest.NewUnstartedServer(mux)
	ts.TLS = peerMesh.ServerTLSConfig()
	ts.StartTLS()
	t.Cleanup(ts.Close)

	_, portStr, err := net.SplitHostPort(ts.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse listener port: %v", err)
	}
	// "localhost" is not a loopback IP literal, so the read is a peer's read
	// and not this node's own engine-manager.
	return "localhost", port, stub
}

// newClusteredTestManager returns a manager that believes it is a clustered
// node, with its engine-manager port pointed at the peer stub.
func newClusteredTestManager(t *testing.T, selfDir string, emPort int) *Manager {
	t.Helper()
	rw := newCaptureRW()
	mesh := clustertrust.Open(selfDir)
	m, err := NewManager(NewCodec(rw), tlsClientOptions{}, mesh)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	m.emPort = emPort
	// The plain-HTTP and operator-TLS fallbacks must not answer: a peer is
	// read over cluster mTLS, and a weaker transport is not a view.
	m.client = &http.Client{Transport: newFakeRoundTripper()}
	m.tlsClient = &http.Client{Transport: newFakeRoundTripper()}
	return m
}

// waitForProbedNode waits until the node's status carries the peer's model
// list, which means the probe that reads the peer's engine-manager surface
// has finished, and returns the status it settled on.
//
// Adding a node probes it in the background, so a test that probes again
// explicitly overlaps that probe; the status converges either way, and waiting
// for the probe to finish is what makes the assertion deterministic.
func waitForProbedNode(t *testing.T, m *Manager, id string) ManualNodeStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.RLock()
		tn, ok := m.nodes[id]
		var status ManualNodeStatus
		if ok {
			status = tn.status
		}
		m.mu.RUnlock()
		if ok && len(status.Models) > 0 {
			return status
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the node's probe did not finish in time")
	return ManualNodeStatus{}
}

// readEngineNativeNow forces the node's engine view to be due again and reads
// it, so a test can exercise a failing read rather than the cadence.
func readEngineNativeNow(t *testing.T, m *Manager, id string) *enginenative.Stats {
	t.Helper()
	m.mu.Lock()
	tn, ok := m.nodes[id]
	if ok {
		tn.lastEngineNativeAt = time.Time{}
	}
	m.mu.Unlock()
	m.probeNode(m.nodes[id].entry)
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.nodes[id].status.EngineNative
}

// TestManualNodeCarriesThePeerEngineNativeView is the regression this fix
// exists for: a peer this host reaches by address (a manual node) carries the
// engine's own view, read over the same pinned mTLS path the model list beside
// it is read over.
//
// Before the fix the manual-node prober read only the model list, so a manual
// node's record carried no engineNative at all and the node card showed no
// engine numbers.
func TestManualNodeCarriesThePeerEngineNativeView(t *testing.T) {
	selfDir := newClusterSelfDir(t)
	host, port, stub := startPeerEngineManager(t, selfDir, engineNativeBody())

	m := newClusteredTestManager(t, selfDir, port)
	entry := ManualEntry{Address: host, Name: "peer"}
	m.addNode(entry)

	status := waitForProbedNode(t, m, nodeID(entry))
	if status.EngineNative == nil {
		t.Fatal("a manual peer carries no engine-native view")
	}
	if status.EngineNative.Engine != enginenative.EngineOmlx || !status.EngineNative.Supported {
		t.Fatalf("engine-native view = %+v, want the omlx view", status.EngineNative)
	}
	if status.EngineNative.Totals.Requests != 2 || status.EngineNative.Totals.PromptTokens != 120 {
		t.Errorf("engine-native totals = %+v, want the engine's own counters", status.EngineNative.Totals)
	}
	if status.EngineNative.Rates.GenerationTokensPerSecond != 50 {
		t.Errorf("engine-native rates = %+v, want the engine's own rates", status.EngineNative.Rates)
	}
	if len(status.EngineNative.Models) != 1 || !status.EngineNative.Models[0].Loaded {
		t.Errorf("engine-native models = %+v, want the engine's own list", status.EngineNative.Models)
	}
	if status.EngineNative.ObservedAt != 1792663511584 {
		t.Errorf("observation stamp = %d, want the reading's own stamp", status.EngineNative.ObservedAt)
	}
	// The view is cluster data: it was read over the pinned mTLS path, not in
	// plaintext and not over the operator's TLS client.
	if !stub.sawClientCert() {
		t.Error("the engine's view was not read over cluster mTLS")
	}
	if stub.sawPlaintext() {
		t.Error("the engine's view was read in plaintext")
	}
	// The model list beside it is unchanged: the two are separate reads.
	if len(status.Models) != 1 || status.Models[0] != "a-model" {
		t.Errorf("model list = %v, want the peer's own list", status.Models)
	}
}

// TestManualNodeWithNoEngineViewCarriesNone pins that a node whose engine
// publishes no admin API carries no view: the em surface answers
// "unsupported", which is not a view of the engine.
func TestManualNodeWithNoEngineViewCarriesNone(t *testing.T) {
	selfDir := newClusterSelfDir(t)
	host, port, _ := startPeerEngineManager(t, selfDir, map[string]any{"engine": "omlx", "supported": false})

	m := newClusteredTestManager(t, selfDir, port)
	entry := ManualEntry{Address: host, Name: "peer"}
	m.addNode(entry)

	status := waitForProbedNode(t, m, nodeID(entry))
	if status.EngineNative != nil {
		t.Fatalf("a node whose engine has no admin API carries a view: %+v", status.EngineNative)
	}
}

// TestManualNodeKeepsTheLastGoodEngineView pins the last-good rule: a node
// whose engine view can no longer be read keeps the view it had, while a node
// that never produced a view has none.
func TestManualNodeKeepsTheLastGoodEngineView(t *testing.T) {
	selfDir := newClusterSelfDir(t)
	host, port, stub := startPeerEngineManager(t, selfDir, engineNativeBody())

	m := newClusteredTestManager(t, selfDir, port)
	entry := ManualEntry{Address: host, Name: "peer"}
	m.addNode(entry)

	status := waitForProbedNode(t, m, nodeID(entry))
	before := status.EngineNative
	if before == nil {
		t.Fatal("a manual peer carries no engine-native view")
	}

	// The engine stops answering, and the view is due for a fresh reading.
	stub.set(map[string]any{"engine": "omlx", "supported": false})
	after := readEngineNativeNow(t, m, nodeID(entry))

	if after == nil {
		t.Fatal("a failed read deleted the engine-native view")
	}
	if !enginenative.SameNumbers(before, after) {
		t.Errorf("engine-native view = %+v, want the last good one %+v", after, before)
	}
}
