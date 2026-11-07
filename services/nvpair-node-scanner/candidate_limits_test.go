// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net"
	"nvpair-shared/noderec"
	"strconv"
	"testing"
	"time"
)

// Liveness consumes a previously established endpoint binding, independently of
// cryptographic inventory tests, which establish it using real pinned mTLS.
func recordProvenEndpoint(d *daemon, raw RawNode) {
	node, _ := toDirectoryNode(raw)
	node.IdentityVerification = "verified"
	node.Trusted = true
	node.PeerRecordID = "test-proof"
	d.dir.upsert(node)
}

func TestLookalikeFloodIsBoundedAndProvenPeerStillVerifies(t *testing.T) {
	d, srv, _ := secureInventoryFixture(t, "uuid-peer")
	host, p, _ := net.SplitHostPort(srv.Listener.Addr().String())
	genuine := RawNode{ID: "genuine", Host: "genuine.local.", Port: 14318, Addresses: []string{host}, TXT: []string{"uuid=uuid-peer", "cluster-uuid=uuid-peer", "ip=" + host, "ni=" + p}}
	d.onBrowse(DiscoveryEvent{Type: "discovered", Node: genuine})
	peer, ok := d.dir.get("uuid-peer")
	if !ok || !peer.ServicePeer() {
		t.Fatal("initial proof failed")
	}
	browser := d.newCandidateBrowser()
	browser.Seed(genuine)
	for i := range 2000 {
		fake := RawNode{ID: fmt.Sprintf("fake-%d", i), Host: "lookalike.local.", TXT: []string{"uuid=uuid-peer", "cluster-uuid=uuid-peer", "ip=127.0.0.2", "ni=1"}}
		browser.Seed(fake)
		d.onBrowse(DiscoveryEvent{Type: "discovered", Node: fake})
	}
	if len(browser.Nodes()) > candidatePerClaimLimit || len(d.candidateSnapshot()) > candidatePerClaimLimit {
		t.Fatal("same-UUID candidate flood exceeded bound")
	}
	for i := range 1000 {
		raw := RawNode{ID: fmt.Sprintf("other-%d", i), TXT: []string{fmt.Sprintf("uuid=other-%d", i)}}
		browser.Seed(raw)
		d.onBrowse(DiscoveryEvent{Type: "discovered", Node: raw})
	}
	if len(browser.Nodes()) > candidateTotalLimit || len(d.candidateSnapshot()) > candidateTotalLimit || len(d.candidateAttempts) > candidateTotalLimit {
		t.Fatal("global flood exceeded bound")
	}
	peer.LastSeen = time.Now().Add(-time.Minute).Unix()
	d.dir.upsert(peer)
	// The reserved proven-endpoint attempt works even after lookalikes exhaust
	// their UUID's budget and unrelated UUIDs fill the global window table.
	d.onBrowse(DiscoveryEvent{Type: "updated", Node: genuine})
	after, _ := d.dir.get("uuid-peer")
	if !after.ServicePeer() || after.LastSeen <= peer.LastSeen {
		t.Fatal("flood starved genuine pinned-mTLS reauthentication")
	}
	found := false
	for _, n := range browser.Nodes() {
		found = found || n.ID == genuine.ID
	}
	if !found {
		t.Fatal("browser lost the previously proven advertisement")
	}
}

func TestCandidateAttemptBudgetSurvivesRemovalAndResetsAfterWindow(t *testing.T) {
	d := newSelfTestDaemon("self", "127.0.0.1")
	now := time.Now()
	for i := range candidatePerClaimLimit {
		raw := RawNode{ID: fmt.Sprintf("fake-%d", i), TXT: []string{"uuid=peer"}}
		n, _ := toDirectoryNode(raw)
		if !d.claimCandidateAttempt(n, now) {
			t.Fatal("budget ended early")
		}
		d.onBrowse(DiscoveryEvent{Type: "removed", Node: raw})
	}
	n, _ := toDirectoryNode(RawNode{ID: "new-name", TXT: []string{"uuid=peer"}})
	if d.claimCandidateAttempt(n, now) {
		t.Fatal("removal/name churn reset per-UUID budget")
	}
	if !d.claimCandidateAttempt(n, now.Add(candidateAttemptInterval)) {
		t.Fatal("budget did not refill")
	}
}

func TestGlobalCandidateAttemptBudgetAndClaimMutation(t *testing.T) {
	d := newSelfTestDaemon("self", "127.0.0.1")
	now := time.Now()
	admitted := 0
	for i := range 1000 {
		raw := RawNode{ID: fmt.Sprintf("candidate-%d", i), TXT: []string{fmt.Sprintf("uuid=peer-%d", i)}}
		node, _ := toDirectoryNode(raw)
		if d.claimCandidateAttempt(node, now) {
			admitted++
		}
	}
	if admitted != candidateGlobalAttempts {
		t.Fatalf("global attempts = %d", admitted)
	}
	for i := range candidatePerClaimLimit {
		n, _ := toDirectoryNode(RawNode{ID: fmt.Sprintf("same-%d", i), TXT: []string{"uuid=one"}})
		if !d.rememberCandidate(n, "") {
			t.Fatal("candidate bound ended early")
		}
	}
	raw := RawNode{ID: "changing", TXT: []string{"uuid=two"}}
	old, _ := toDirectoryNode(raw)
	if !d.rememberCandidate(old, "") {
		t.Fatal("second claim rejected early")
	}
	d.dir.upsert(old)
	changed := old
	changed.HostUUID = "one"
	if d.rememberCandidate(changed, "") {
		t.Fatal("UUID mutation bypassed per-claim bound")
	}
	if _, exists := d.dir.get(old.RecordKey()); exists {
		t.Fatal("rejected claim mutation left old preview")
	}
	if len(d.candidateSnapshot()) != candidatePerClaimLimit {
		t.Fatal("candidate leak after claim mutation")
	}
}

func TestCandidateAddressLimitMatchesProductionPipeline(t *testing.T) {
	d := newSelfTestDaemon("self", "127.0.0.1")
	raw := RawNode{ID: "peer", TXT: []string{"uuid=peer", "ips=192.0.2.1,192.0.2.2,192.0.2.3,192.0.2.4,192.0.2.5,192.0.2.6"}}
	node, ok := toDirectoryNode(raw)
	if !ok || len(node.CandidateIPs()) != noderec.MaxAdvertisedIPs {
		t.Fatal("production address cap not applied")
	}
	hosts := node.CandidateIPs()
	seen := make(map[string]bool)
	for _, now := range []time.Time{time.Unix(0, 0), time.Unix(15, 0)} {
		batch := d.candidateHosts(noderec.DirectoryNode{}, hosts, now)
		if len(batch) > 4 {
			t.Fatal("address-attempt bound exceeded")
		}
		for _, host := range batch {
			seen[host] = true
		}
	}
	if len(seen) != len(hosts) {
		t.Fatal("an admitted production address was discarded")
	}
	// This is an input bound, not evidence that the fifth/sixth advertised
	// addresses rotate back into the production candidate list.
	if seen["192.0.2.5"] || seen["192.0.2.6"] {
		t.Fatal("production pipeline bypassed netpick address cap")
	}
}

func TestLookalikeCannotBorrowVerifiedLiveness(t *testing.T) {
	d := newSelfTestDaemon("self", "127.0.0.1")
	port := closedPort(t)
	genuine := RawNode{ID: "genuine", TXT: []string{"uuid=peer", "ip=127.0.0.1", "ni=" + strconv.Itoa(port)}}
	recordProvenEndpoint(d, genuine)
	d.lastInfoAt["peer"] = time.Now()
	d.lastActivityAt = make(map[string]time.Time)
	d.noteActivity("peer", 0)
	if !d.reachable(genuine) {
		t.Fatal("proven endpoint did not retain anti-flap protection")
	}
	lookalike := RawNode{ID: "lookalike", TXT: []string{"uuid=peer", "ip=127.0.0.2", "ni=" + strconv.Itoa(port)}}
	if d.reachable(lookalike) {
		t.Fatal("UUID claim borrowed another advertisement's cached liveness")
	}
	// The lookalike's eviction cannot remove the proven inventory.
	d.onBrowse(DiscoveryEvent{Type: "removed", Node: lookalike})
	if peer, ok := d.dir.get("peer"); !ok || !peer.ServicePeer() {
		t.Fatal("lookalike removal withdrew proven peer")
	}
}
