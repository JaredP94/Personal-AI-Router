// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/inferencemetrics"
)

// progressPeer is a peer node running the inter-node server, wired the way
// the cluster wires it: the peer's own mTLS identity serves the events
// endpoint, and this node holds the pin for it.
type progressPeer struct {
	manager *Manager
	server  *httptest.Server

	mu       sync.Mutex
	relayed  []json.RawMessage
	upserts  int
	removals int
}

// newProgressPeer stands up a peer that records what the inter-node path
// relays to its broker, and returns a manager on this node whose broadcaster
// addresses that peer.
func newProgressPeer(t *testing.T) *progressPeer {
	t.Helper()
	self, peer := newPinnedPeerMeshes(t)

	p := &progressPeer{}
	srv := NewServer(0, newDedupIndex(64), peer,
		func(*Workload) error { p.mu.Lock(); p.upserts++; p.mu.Unlock(); return nil },
		func(string, string) error { p.mu.Lock(); p.removals++; p.mu.Unlock(); return nil },
		func(params json.RawMessage) error {
			p.mu.Lock()
			p.relayed = append(p.relayed, params)
			p.mu.Unlock()
			return nil
		})

	mux := http.NewServeMux()
	mux.HandleFunc(eventsPath, srv.handleEvents)
	ts := httptest.NewUnstartedServer(mux)
	ts.TLS = peer.ServerTLSConfig()
	ts.StartTLS()
	t.Cleanup(ts.Close)
	p.server = ts

	host, portStr, err := net.SplitHostPort(ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	peers := newPeerSet(port)
	peers.Replace([]PeerNode{{
		ID:        "peer-1",
		Host:      host,
		Addresses: []string{host},
		Port:      port,
		TXT:       []string{clustertrust.ClusterUUIDTXTKey + "=uuid-peer"},
	}})

	m := &Manager{
		dedup:       newDedupIndex(64),
		peers:       peers,
		broadcaster: NewBroadcaster(peers, self),
		activeLocal: make(map[workloadKey]workloadEvent),
		ctx:         context.Background(),
	}
	t.Cleanup(m.broadcaster.CloseIdle)
	p.manager = m
	return p
}

// relayedFrames returns the frames the peer relayed to its broker.
func (p *progressPeer) relayedFrames() []json.RawMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]json.RawMessage(nil), p.relayed...)
}

// waitForFrames waits until the peer has relayed at least want frames, or the
// deadline passes. The broadcast is asynchronous, so the test has to wait for
// it rather than assume it landed.
func (p *progressPeer) waitForFrames(t *testing.T, want int) []json.RawMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if frames := p.relayedFrames(); len(frames) >= want {
			return frames
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the peer relayed %d frame(s), want %d", len(p.relayedFrames()), want)
	return nil
}

// progressParams builds the params of a live progress frame.
func progressParams(t *testing.T, frame inferencemetrics.ProgressFrame) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestProgressFrameIsBroadcastToPeers is the cluster reach of a live frame:
// the broker hands the manager a frame, the manager broadcasts it to the
// pinned peers, and the peer's inter-node server relays it to the peer's
// broker — so a desktop on the peer sees the job's live numbers.
func TestProgressFrameIsBroadcastToPeers(t *testing.T) {
	peer := newProgressPeer(t)

	frame := inferencemetrics.ProgressFrame{
		JobID:     "17",
		NodeID:    "node-b",
		ElapsedMs: inferencemetrics.Int64Ptr(4200),
		Chunks:    inferencemetrics.Int64Ptr(34),
		Bytes:     inferencemetrics.Int64Ptr(1820),
	}
	peer.manager.handleMessage(&Message{
		JSONRPC: "2.0",
		Method:  MethodProgress,
		Params:  progressParams(t, frame),
	})

	frames := peer.waitForFrames(t, 1)
	var got inferencemetrics.ProgressFrame
	if err := json.Unmarshal(frames[0], &got); err != nil {
		t.Fatalf("relayed frame is not a progress frame: %v", err)
	}
	if got.JobID != "17" || got.NodeID != "node-b" {
		t.Fatalf("relayed frame = %+v, want job 17 on node-b", got)
	}
	if got.ElapsedMs == nil || *got.ElapsedMs != 4200 {
		t.Fatalf("relayed elapsed = %v, want 4200", got.ElapsedMs)
	}
	if got.Chunks == nil || *got.Chunks != 34 {
		t.Fatalf("relayed chunks = %v, want 34", got.Chunks)
	}
}

// TestTwoProgressFramesForOneJobAreBothBroadcast pins the rule that makes a
// live view live: a progress frame is not a state transition, so the dedup
// that swallows a repeated lifecycle event must not swallow it. Two frames
// for the same job id are two readings, and the second one carries the newer
// numbers.
func TestTwoProgressFramesForOneJobAreBothBroadcast(t *testing.T) {
	peer := newProgressPeer(t)

	first := inferencemetrics.ProgressFrame{JobID: "17", NodeID: "node-b", ElapsedMs: inferencemetrics.Int64Ptr(2000)}
	second := inferencemetrics.ProgressFrame{JobID: "17", NodeID: "node-b", ElapsedMs: inferencemetrics.Int64Ptr(4000)}
	peer.manager.handleMessage(&Message{JSONRPC: "2.0", Method: MethodProgress, Params: progressParams(t, first)})
	peer.manager.handleMessage(&Message{JSONRPC: "2.0", Method: MethodProgress, Params: progressParams(t, second)})

	frames := peer.waitForFrames(t, 2)
	var elapsed []int64
	for _, raw := range frames {
		var got inferencemetrics.ProgressFrame
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("relayed frame is not a progress frame: %v", err)
		}
		if got.ElapsedMs == nil {
			t.Fatalf("relayed frame carries no elapsed time: %+v", got)
		}
		elapsed = append(elapsed, *got.ElapsedMs)
	}
	// Both readings must be there; the order they land in is not the point.
	seen := map[int64]bool{}
	for _, e := range elapsed {
		seen[e] = true
	}
	if !seen[2000] || !seen[4000] {
		t.Fatalf("relayed elapsed times = %v, want both 2000 and 4000 (a frame must not be deduped away)", elapsed)
	}
}

// TestProgressFrameEntersNoCatalog: a live frame is a snapshot of a running
// request, not a lifecycle event. It must not be tracked as an active
// workload (the set the heartbeat re-asserts) and it must not reach the
// catalog as an upsert.
func TestProgressFrameEntersNoCatalog(t *testing.T) {
	peer := newProgressPeer(t)

	frame := inferencemetrics.ProgressFrame{JobID: "17", NodeID: "node-b", ElapsedMs: inferencemetrics.Int64Ptr(4200)}
	peer.manager.handleMessage(&Message{JSONRPC: "2.0", Method: MethodProgress, Params: progressParams(t, frame)})
	peer.waitForFrames(t, 1)

	peer.manager.activeMu.Lock()
	tracked := len(peer.manager.activeLocal)
	peer.manager.activeMu.Unlock()
	if tracked != 0 {
		t.Fatalf("the re-sync set holds %d entries, want 0 (a frame is not a lifecycle event)", tracked)
	}
	if snap := peer.manager.activeSnapshot(); len(snap) != 0 {
		t.Fatalf("the re-sync snapshot holds %d entries, want 0", len(snap))
	}
	peer.mu.Lock()
	upserts, removals := peer.upserts, peer.removals
	peer.mu.Unlock()
	if upserts != 0 || removals != 0 {
		t.Fatalf("the peer catalog saw %d upsert(s) and %d removal(s), want none", upserts, removals)
	}
}

// TestMalformedProgressFrameIsNotBroadcast: a frame that names no job is not
// a frame — it would land in no job's view — so it is dropped rather than
// broadcast.
func TestMalformedProgressFrameIsNotBroadcast(t *testing.T) {
	peer := newProgressPeer(t)

	peer.manager.handleMessage(&Message{
		JSONRPC: "2.0",
		Method:  MethodProgress,
		Params:  json.RawMessage(`{"nodeId":"node-b","elapsedMs":4200}`),
	})
	peer.manager.handleMessage(&Message{
		JSONRPC: "2.0",
		Method:  MethodProgress,
		Params:  json.RawMessage(`not json`),
	})

	// Give the (absent) broadcast time to land before asserting it did not.
	time.Sleep(200 * time.Millisecond)
	if frames := peer.relayedFrames(); len(frames) != 0 {
		t.Fatalf("the peer relayed %d malformed frame(s), want 0", len(frames))
	}
}
