// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net"
	"strconv"
	"testing"
	"time"

	"nvpair-shared/noderec"
)

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
