// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"testing"
	"time"

	"nvpair-shared/noderec"
)

// A refresh in the same timestamp second can change inventory without changing
// the proof, IP or LastSeen. Expiry must compare the entire observed record.
func TestVerifiedExpiryPreservesConcurrentRefresh(t *testing.T) {
	h := newTelemetryHarness(t)
	h.peer("peer", func(*http.Request) NodeInfoResponse { return NodeInfoResponse{HostUUID: "peer"} })
	old, _ := h.d.dir.get("peer")
	fresh := old
	fresh.Models = []string{"fresh-model"}
	capture := &notifyCapture{}
	h.d.codec = NewCodec(capture)
	// Hold the directory lock while expiry starts with its old snapshot, then
	// publish the refreshed record before permitting its conditional removal.
	h.d.dir.mu.Lock()
	started, done := make(chan struct{}), make(chan struct{})
	go func() { close(started); h.d.expireVerified(old); close(done) }()
	<-started
	h.d.dir.nodes["peer"] = fresh
	h.d.dir.mu.Unlock()
	<-done
	got, ok := h.d.dir.get("peer")
	if !ok || len(got.Models) != 1 || got.Models[0] != "fresh-model" {
		t.Fatalf("stale expiry deleted refreshed inventory: %+v, exists=%v", got, ok)
	}
	if len(capture.nodes(t)) != 0 {
		t.Fatal("stale expiry withdrew refreshed peer telemetry")
	}
}

func TestVerifiedExpiryOrdersWithdrawalBeforeNewPublication(t *testing.T) {
	h := newTelemetryHarness(t)
	h.peer("peer", func(*http.Request) NodeInfoResponse { return NodeInfoResponse{HostUUID: "peer"} })
	old, _ := h.d.dir.get("peer")
	fresh := old
	fresh.Models = []string{"fresh-model"}
	capture := &notifyCapture{}
	h.d.codec = NewCodec(capture)
	// Pause expiry after directory removal but before cache cleanup/withdrawal.
	h.d.infoMu.Lock()
	expired := make(chan struct{})
	go func() { h.d.expireVerified(old); close(expired) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, exists := h.d.dir.get("peer"); !exists {
			break
		}
		if time.Now().After(deadline) {
			h.d.infoMu.Unlock()
			t.Fatal("expiry did not remove old record")
		}
		runtime.Gosched()
	}
	published := make(chan struct{})
	go func() { h.d.publishVerified(fresh, ""); close(published) }()
	select {
	case <-published:
		t.Error("publication overtook paused withdrawal")
	case <-time.After(50 * time.Millisecond):
	}
	h.d.infoMu.Unlock()
	<-expired
	<-published
	capture.mu.Lock()
	raw := bytes.Clone(capture.frames)
	capture.mu.Unlock()
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("expected withdrawal and update, got %s", raw)
	}
	for i, want := range []string{noderec.NotifyNodeRemoved, noderec.NotifyNodeUpdated} {
		var msg Message
		if err := json.Unmarshal(lines[i], &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Method != want {
			t.Errorf("event %d=%s, want %s; fresh inventory would be withdrawn", i, msg.Method, want)
		}
	}
	if got, ok := h.d.dir.get("peer"); !ok || len(got.Models) != 1 || got.Models[0] != "fresh-model" {
		t.Fatal("new publication lost")
	}
}

// Browser churn must not reset the independent verified peer's retry budget.
func TestBrowseChurnPreservesVerifiedTelemetryRetry(t *testing.T) {
	d, server, _ := secureInventoryFixture(t, "uuid-peer")
	host, portString, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.Atoi(portString)
	raw := RawNode{ID: "genuine", Addresses: []string{host}, TXT: []string{
		"v=1", "uuid=uuid-peer", "cluster-uuid=uuid-peer", "ip=" + host, "ni=" + portString,
	}}
	d.onBrowse(DiscoveryEvent{Type: "discovered", Node: raw})
	if node, ok := d.dir.get("uuid-peer"); !ok || !node.ServicePeer() {
		t.Fatal("fixture did not establish an authenticated peer")
	}
	now := time.Now()
	target := telemetryTargetKey([]string{host}, port)
	token, ok := d.telemetryRetries.claim("uuid-peer", target, now)
	if !ok {
		t.Fatal("initial telemetry claim denied")
	}
	d.telemetryRetries.finish("uuid-peer", token, false, now)
	raw.TXT = append(raw.TXT, "display-hint=changed")
	d.onBrowse(DiscoveryEvent{Type: "updated", Node: raw})
	if _, ok := d.telemetryRetries.claim("uuid-peer", target, now); ok {
		t.Fatal("TXT refresh cleared verified telemetry backoff")
	}
	d.onBrowse(DiscoveryEvent{Type: "removed", Node: raw})
	if _, ok := d.telemetryRetries.claim("uuid-peer", target, now); ok {
		t.Fatal("candidate removal cleared verified telemetry backoff")
	}
	if node, ok := d.dir.get("uuid-peer"); !ok || !node.ServicePeer() {
		t.Fatal("candidate removal withdrew authenticated inventory")
	}
}

// A wipe replaces the key-bearing endpoint, not merely a hostUuid field under
// the old certificate. An answering TCP port and a new public identity must not
// extend the independent verified record's lifetime.
func TestVerifiedPeerWipedAtSameAddressExpiresDespitePublicResponder(t *testing.T) {
	d, server, _ := secureInventoryFixture(t, "uuid-peer")
	addr := server.Listener.Addr().String()
	host, portString, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portString)
	raw := RawNode{ID: "genuine", Addresses: []string{host}, TXT: []string{
		"v=1", "uuid=uuid-peer", "cluster-uuid=uuid-peer", "ip=" + host, "ni=" + portString,
	}}
	d.onBrowse(DiscoveryEvent{Type: "discovered", Node: raw})
	original, ok := d.dir.get("uuid-peer")
	if !ok || !original.ServicePeer() {
		t.Fatal("fixture did not establish an authenticated peer")
	}
	// A multicast miss alone must not withdraw the proven incarnation.
	d.onBrowse(DiscoveryEvent{Type: "removed", Node: raw})
	d.refreshNodeClusterIdentity("uuid-peer", host, port)
	if node, ok := d.dir.get("uuid-peer"); !ok || !node.ServicePeer() {
		t.Fatal("multicast loss evicted a peer that still proves its identity")
	}
	server.Close()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	wiped := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(NodeInfoResponse{HostUUID: "new-uuid-after-wipe"})
	})}
	t.Cleanup(func() { _ = wiped.Close() })
	go func() { _ = wiped.Serve(listener) }()
	if info, ok := d.probeIdentity(host, port); !ok || info.HostUUID != "new-uuid-after-wipe" {
		t.Fatalf("wiped endpoint must remain live under a fresh identity: %+v, ok=%v", info, ok)
	}

	capture := &notifyCapture{}
	d.codec = NewCodec(capture)
	d.refreshNodeClusterIdentity("uuid-peer", host, port)
	if _, ok := d.dir.get("uuid-peer"); !ok || len(capture.nodes(t)) != 0 {
		t.Fatal("one failed proof must not bypass the verified-peer grace period")
	}
	stale, _ := d.dir.get("uuid-peer")
	stale.LastSeen = time.Now().Add(-time.Minute).Unix()
	d.dir.upsert(stale)
	d.refreshNodeClusterIdentity("uuid-peer", host, port)
	if _, ok := d.dir.get("uuid-peer"); ok {
		t.Fatal("a new identity at the same live endpoint kept stale verified inventory alive")
	}
	events := capture.nodes(t)
	if len(events) != 1 || events[0].HostUUID != "uuid-peer" || events[0].PeerRecordID != original.PeerRecordID || events[0].IdentityVerification != "verified" {
		t.Fatalf("withdrawal did not name the original verified record: %+v", events)
	}
}

func TestFailedProofExpiresVerifiedRecordWithOriginalToken(t *testing.T) {
	d, server, mismatch := secureInventoryFixture(t, "uuid-peer")
	host, portString, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.Atoi(portString)
	d.onBrowse(DiscoveryEvent{Type: "discovered", Node: RawNode{ID: "genuine", Addresses: []string{host}, TXT: []string{
		"v=1", "uuid=uuid-peer", "cluster-uuid=uuid-peer", "ip=" + host, "ni=" + portString,
	}}})
	node, ok := d.dir.get("uuid-peer")
	if !ok || !node.ServicePeer() {
		t.Fatal("fixture did not establish an authenticated peer")
	}
	proof := node.PeerRecordID
	node.LastSeen = time.Now().Add(-time.Minute).Unix()
	d.dir.upsert(node)
	capture := &notifyCapture{}
	d.codec = NewCodec(capture)
	mismatch.Store(true)
	d.refreshNodeClusterIdentity("uuid-peer", host, port)
	if _, exists := d.dir.get("uuid-peer"); exists {
		t.Fatal("failed proof retained stale verified inventory")
	}
	events := capture.nodes(t)
	if len(events) != 1 || events[0].HostUUID != "uuid-peer" || events[0].PeerRecordID != proof || events[0].IdentityVerification != "verified" {
		t.Fatalf("withdrawal lost original proof identity: %+v", events)
	}
	var removed bool
	for _, n := range d.dir.snapshot("") {
		removed = removed || n.RecordKey() == noderec.PreviewRecordID("genuine", "uuid-peer")
	}
	if removed {
		t.Fatal("withdrawal incorrectly relabelled old proof as a public preview")
	}
}
