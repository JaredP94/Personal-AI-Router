// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/noderec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Scheduling tests use actual pinned TLS, not public inventory fixtures. The
// initial directory record represents an already authenticated discovery; each
// telemetry request must independently prove its certificate and response ID.
type telemetryHarness struct {
	t       *testing.T
	d       *daemon
	selfDir string
}

func newTelemetryHarness(t *testing.T) *telemetryHarness {
	t.Helper()
	dir := t.TempDir()
	clustertrusttest.Join(t, dir, "telemetry-cluster", "self")
	d := newSelfTestDaemon("self", "192.0.2.1")
	d.mesh = clustertrust.Open(dir)
	return &telemetryHarness{t: t, d: d, selfDir: dir}
}
func (h *telemetryHarness) peer(uuid string, body func(*http.Request) NodeInfoResponse) (string, int) {
	t := h.t
	t.Helper()
	dir := t.TempDir()
	clustertrusttest.Join(t, dir, "telemetry-cluster", uuid)
	writePinFromCert(t, h.selfDir, uuid, filepath.Join(dir, "node.crt"))
	writePinFromCert(t, dir, "self", filepath.Join(h.selfDir, "node.crt"))
	mesh := clustertrust.Open(dir)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := mesh.VerifyClientPin(r); !ok {
			http.Error(w, "forbidden", 403)
			return
		}
		response := body(r)
		_, p, _ := net.SplitHostPort(r.Host)
		port, _ := strconv.Atoi(p)
		response.ClusterUUID = &uuid
		response.PeerMetadata = &noderec.PeerMetadataParams{HostUUID: uuid, Name: uuid, Services: map[noderec.ServiceKey]int{noderec.ServiceNodeInfo: port}}
		_ = json.NewEncoder(w).Encode(response)
	}))
	srv.TLS = mesh.ServerTLSConfig()
	srv.StartTLS()
	t.Cleanup(srv.Close)
	host, p, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(p)
	h.d.mesh.Refresh()
	h.d.dir.upsert(noderec.DirectoryNode{HostUUID: uuid, ClusterUUID: uuid, Name: uuid, IP: host, Trusted: true, IdentityVerification: "verified", PeerRecordID: h.d.mesh.PeerRecordID(uuid), Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceNodeInfo: {Port: port}}})
	return host, port
}

func TestRefreshNodeTelemetryEmitsFreshnessAndUtilization(t *testing.T) {
	h := newTelemetryHarness(t)
	host, port := h.peer("node-a", func(*http.Request) NodeInfoResponse {
		return NodeInfoResponse{HostUUID: "node-a", TelemetryValid: true, MSSince: 137, SampleSeq: 42, GPUs: []GPUInfo{{Name: "GPU 0", UtilizationPercent: gpuReading(uint32(0))}, {Name: "GPU 1", UtilizationPercent: gpuReading(uint32(84))}}}
	})
	capture := &notifyCapture{}
	h.d.codec = NewCodec(capture)
	if !h.d.refreshNodeTelemetry(t.Context(), "node-a", host, port) {
		t.Fatal("pinned telemetry rejected")
	}
	capture.mu.Lock()
	data := append([]byte(nil), capture.frames...)
	capture.mu.Unlock()
	var message Message
	if err := json.Unmarshal(data, &message); err != nil {
		t.Fatal(err)
	}
	var got noderec.NodeTelemetry
	if err := json.Unmarshal(message.Params, &got); err != nil {
		t.Fatal(err)
	}
	if message.Method != noderec.NotifyNodeTelemetry || got.HostUUID != "node-a" || !got.TelemetryValid || got.GPUUtilizationPct != 84 || got.SampleSeq != 42 || got.PeerRecordID == "" || got.MSSince < 137 || got.MSSince >= 3500 {
		t.Fatalf("telemetry = %+v", got)
	}
}
func TestRefreshNodeTelemetryRejectsMismatchedIdentity(t *testing.T) {
	h := newTelemetryHarness(t)
	host, port := h.peer("node-a", func(*http.Request) NodeInfoResponse {
		return NodeInfoResponse{HostUUID: "node-b", TelemetryValid: true}
	})
	if h.d.refreshNodeTelemetry(t.Context(), "node-a", host, port) {
		t.Fatal("mismatched response accepted")
	}
}
func TestRefreshTelemetryFailsOverToAnAnsweringAddress(t *testing.T) {
	h := newTelemetryHarness(t)
	host, port := h.peer("node-a", func(*http.Request) NodeInfoResponse {
		return NodeInfoResponse{HostUUID: "node-a", TelemetryValid: true}
	})
	if !h.d.refreshNodeTelemetryCandidates(t.Context(), "node-a", []string{"127.0.0.2", host}, port) {
		t.Fatal("answering second address ignored")
	}
}
func TestTelemetryIntervalForNodeIsStableAndBounded(t *testing.T) {
	if telemetryIntervalForNode("") != telemetryRefreshInterval {
		t.Fatal("default interval changed")
	}
	first := telemetryIntervalForNode("node-a")
	if first != telemetryIntervalForNode("node-a") || first < telemetryRefreshInterval-telemetryRefreshJitter || first > telemetryRefreshInterval+telemetryRefreshJitter || first == telemetryIntervalForNode("node-b") {
		t.Fatal("jitter is unstable or out of bounds")
	}
}
func TestTelemetryRetryDelay(t *testing.T) {
	for _, tc := range []struct {
		failures int
		want     time.Duration
	}{{0, 2 * time.Second}, {1, 4 * time.Second}, {2, 8 * time.Second}, {3, 16 * time.Second}, {4, 30 * time.Second}, {100, 30 * time.Second}} {
		if got := telemetryRetryDelay(tc.failures); got != tc.want {
			t.Errorf("delay(%d)=%v, want %v", tc.failures, got, tc.want)
		}
	}
}
func TestTelemetryRetryGateBacksOffAndResets(t *testing.T) {
	var gate telemetryRetryGate
	now := time.Unix(1000, 0)
	key := telemetryTargetKey([]string{"192.0.2.1"}, 14318)
	first, ok := gate.claim("node-a", key, now)
	if !ok || first == 0 {
		t.Fatal("first claim denied")
	}
	if _, ok := gate.claim("node-a", key, now); ok {
		t.Fatal("overlapping claim")
	}
	gate.finish("node-a", first, false, now)
	if _, ok := gate.claim("node-a", key, now.Add(4*time.Second-time.Nanosecond)); ok {
		t.Fatal("early retry")
	}
	second, ok := gate.claim("node-a", key, now.Add(4*time.Second))
	if !ok || second == first {
		t.Fatal("retry not due")
	}
	gate.finish("node-a", second, true, now.Add(4*time.Second))
	if _, ok := gate.claim("node-a", key, now.Add(4*time.Second)); !ok {
		t.Fatal("success did not reset retry")
	}
}
func TestTelemetryRetryGateChangedTargetWaitsForActiveAttempt(t *testing.T) {
	for _, target := range []string{telemetryTargetKey([]string{"10.0.0.1"}, 14318), telemetryTargetKey([]string{"192.0.2.1"}, 14319)} {
		var gate telemetryRetryGate
		now := time.Unix(1500, 0)
		old := telemetryTargetKey([]string{"192.0.2.1"}, 14318)
		token, ok := gate.claim("node-a", old, now)
		if !ok {
			t.Fatal("initial claim denied")
		}
		if _, ok := gate.claim("node-a", target, now); ok {
			t.Fatal("replacement overlapped active request")
		}
		gate.finish("node-a", token, false, now)
		if _, ok := gate.claim("node-a", target, now); !ok {
			t.Fatal("old failure backed off replacement")
		}
	}
}
func TestTelemetryRetryGateRemoveRediscoverPreservesActiveClaim(t *testing.T) {
	var gate telemetryRetryGate
	now := time.Unix(2000, 0)
	key := telemetryTargetKey([]string{"192.0.2.1"}, 14318)
	token, ok := gate.claim("node-a", key, now)
	if !ok {
		t.Fatal("initial claim denied")
	}
	gate.forget("node-a")
	if _, ok := gate.claim("node-a", key, now); ok {
		t.Fatal("rediscovery overlapped old request")
	}
	gate.finish("node-a", token, false, now)
	if _, ok := gate.claim("node-a", key, now); !ok {
		t.Fatal("old failure backed off rediscovery")
	}
}
func TestRefreshTelemetrySkipsBackedOffPeerWithoutDelayingHealthyPeer(t *testing.T) {
	h := newTelemetryHarness(t)
	var backed, healthy atomic.Int32
	host, port := h.peer("backed", func(*http.Request) NodeInfoResponse { backed.Add(1); return NodeInfoResponse{HostUUID: "backed"} })
	h.peer("healthy", func(*http.Request) NodeInfoResponse { healthy.Add(1); return NodeInfoResponse{HostUUID: "healthy"} })
	now := time.Now()
	token, _ := h.d.telemetryRetries.claim("backed", telemetryTargetKey([]string{host}, port), now)
	h.d.telemetryRetries.finish("backed", token, false, now)
	h.d.refreshTelemetryOnce(t.Context())
	if backed.Load() != 0 || healthy.Load() != 1 {
		t.Fatalf("requests: backed=%d healthy=%d", backed.Load(), healthy.Load())
	}
}
func TestTelemetryLoopKeepsHealthyNodeOnCadenceWhilePeerIsBlocked(t *testing.T) {
	h := newTelemetryHarness(t)
	blocked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h.peer("slow", func(*http.Request) NodeInfoResponse {
		once.Do(func() { close(blocked) })
		<-release
		return NodeInfoResponse{HostUUID: "slow"}
	})
	healthy := make(chan struct{}, 20)
	h.peer("healthy", func(*http.Request) NodeInfoResponse {
		healthy <- struct{}{}
		return NodeInfoResponse{HostUUID: "healthy"}
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { h.d.runTelemetryLoop(ctx, 20*time.Millisecond); close(done) }()
	defer func() { cancel(); close(release); <-done }()
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("slow request did not start")
	}
	for i := 0; i < 2; i++ {
		select {
		case <-healthy:
		case <-time.After(2 * time.Second):
			t.Fatal("blocked peer stalled healthy cadence")
		}
	}
}
func TestTelemetryLoopDoesNotOverlapNodePolls(t *testing.T) {
	h := newTelemetryHarness(t)
	var active, maxActive atomic.Int32
	started := make(chan struct{}, 10)
	release := make(chan struct{})
	h.peer("node-a", func(*http.Request) NodeInfoResponse {
		v := active.Add(1)
		if v > maxActive.Load() {
			maxActive.Store(v)
		}
		started <- struct{}{}
		<-release
		active.Add(-1)
		return NodeInfoResponse{HostUUID: "node-a"}
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { h.d.runTelemetryLoop(ctx, 10*time.Millisecond); close(done) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("poll did not start")
	}
	select {
	case <-started:
		t.Error("overlapping request")
	case <-time.After(60 * time.Millisecond):
	}
	cancel()
	close(release)
	<-done
	if maxActive.Load() != 1 {
		t.Fatalf("concurrent polls=%d", maxActive.Load())
	}
}
func TestTelemetryLoopBoundsConcurrentWorkAcrossTicks(t *testing.T) {
	h := newTelemetryHarness(t)
	started := make(chan struct{}, 50)
	release := make(chan struct{})
	for i := 0; i < telemetryRefreshConcurrency+2; i++ {
		uuid := fmt.Sprintf("node-%d", i)
		h.peer(uuid, func(*http.Request) NodeInfoResponse {
			started <- struct{}{}
			<-release
			return NodeInfoResponse{HostUUID: uuid}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	sem := make(chan struct{}, telemetryRefreshConcurrency)
	var workers sync.WaitGroup
	h.d.dispatchTelemetry(ctx, time.Now(), sem, &workers)
	defer func() { cancel(); close(release); workers.Wait() }()
	for i := 0; i < telemetryRefreshConcurrency; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("concurrency budget not filled")
		}
	}
	h.d.dispatchTelemetry(ctx, time.Now(), sem, &workers)
	select {
	case <-started:
		t.Fatal("concurrency cap exceeded")
	case <-time.After(50 * time.Millisecond):
	}
}
