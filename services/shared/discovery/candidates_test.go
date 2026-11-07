// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"fmt"
	"testing"
)

func candidateBrowser(opts ...Option) *Browser {
	return New("_nvpair-node._tcp", "local", append([]Option{WithKeyFunc(AdvertisementKey)}, opts...)...)
}

func TestAdvertisementKeyKeepsSameUUIDCandidatesInBothOrders(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		b := candidateBrowser()
		a, c := node("genuine", "uuid=peer"), node("lookalike", "uuid=peer")
		if reverse {
			a, c = c, a
		}
		b.Seed(a, c)
		if len(b.Nodes()) != 2 {
			t.Fatalf("reverse=%v: retained %d, want both", reverse, len(b.Nodes()))
		}
	}
}

func TestAdvertisementDeduplicatesInterfacesAndAddressOrder(t *testing.T) {
	b := candidateBrowser()
	a := node("peer", "uuid=peer", "ip=192.0.2.1", "ips=192.0.2.1,198.51.100.1")
	a.Addresses = []string{"192.0.2.1"}
	c := a
	c.Addresses = []string{"198.51.100.1"}
	c.TXT = []string{"ips=198.51.100.1,192.0.2.1", "uuid=peer", "ip=198.51.100.1"}
	seen := make(map[string]Node)
	b.observeCandidate(seen, a)
	b.observeCandidate(seen, c)
	b.reconcile(seen)
	if got := b.Nodes(); len(got) != 1 || len(got[0].Addresses) != 2 {
		t.Fatalf("same multi-interface advertisement = %+v", got)
	}
	// Shared instance names must not collapse separate endpoint advertisements.
	c.Host = "another.local."
	b.Seed(c)
	if len(b.Nodes()) != 2 {
		t.Fatal("same-name advertisements collided")
	}
	// A renamed instance is another candidate; authenticated consumers dedup by
	// proven principal, while the previous advertisement ages out independently.
	c.ID = "renamed"
	b.Seed(c)
	if len(b.Nodes()) != 3 {
		t.Fatal("rename displaced another candidate before authentication")
	}
}

func TestCopiedAdvertisementCannotOverwriteServiceOrPrincipalHints(t *testing.T) {
	b := candidateBrowser()
	original := node("same-name", "uuid=peer", "cluster-uuid=peer", "ip=192.0.2.1", "ni=14318")
	b.Seed(original)
	for _, txt := range [][]string{
		{"uuid=peer", "cluster-uuid=peer", "ip=192.0.2.1", "ni=1"},
		{"uuid=peer", "cluster-uuid=lookalike", "ip=192.0.2.1", "ni=14318"},
		{"uuid=lookalike", "cluster-uuid=peer", "ip=192.0.2.1", "ni=14318"},
	} {
		variant := original
		variant.TXT = txt
		b.Seed(variant)
	}
	if len(b.Nodes()) != 4 {
		t.Fatal("a changed service/principal claim overwrote the original record")
	}
	reordered := original
	reordered.TXT = []string{"ni=14318", "ip=192.0.2.1", "cluster-uuid=peer", "uuid=peer"}
	b.Seed(reordered)
	if len(b.Nodes()) != 4 {
		t.Fatal("TXT ordering duplicated the same advertisement")
	}
}

func TestCopiedRecordCannotErasePreviouslyProvenResolvedAddress(t *testing.T) {
	b := candidateBrowser(WithCandidateLimits(8, 256, func(n Node) bool {
		for _, address := range n.Addresses {
			if address == "192.0.2.10" {
				return true
			}
		}
		return false
	}))
	genuine := node("same-name", "uuid=peer", "ip=198.51.100.1")
	genuine.Addresses = []string{"192.0.2.10"}
	b.Seed(genuine)
	copy := genuine
	copy.Addresses = []string{"203.0.113.10"}
	b.reconcile(map[string]Node{b.key(copy): copy})
	got := b.Nodes()
	if len(got) != 1 || got[0].Addresses[0] != "192.0.2.10" {
		t.Fatalf("copied record erased proven address: %+v", got)
	}
}

func TestCandidateMissesAndEvictionArePerAdvertisement(t *testing.T) {
	b := candidateBrowser(WithMissThreshold(2))
	a, c := node("genuine", "uuid=peer"), node("lookalike", "uuid=peer")
	b.Seed(a, c)
	seen := func() map[string]Node { return map[string]Node{b.key(a): a} }
	if evs := b.reconcile(seen()); len(evs) != 0 {
		t.Fatalf("first miss: %v", evs)
	}
	evs := b.reconcile(seen())
	if len(evs) != 1 || evs[0].Type != Removed || evs[0].Node.ID != c.ID {
		t.Fatalf("second miss: %+v", evs)
	}
	if got := b.Nodes(); len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("wrong survivor: %+v", got)
	}
}

func TestCandidateFloodBoundAndProtectedEndpoint(t *testing.T) {
	b := candidateBrowser(WithCandidateLimits(8, 256, func(n Node) bool { return n.ID == "genuine" }))
	genuine := node("genuine", "uuid=peer")
	// Test both a flood preceding the genuine advertisement and following it.
	for _, first := range []bool{false, true} {
		b.nodes = make(map[string]Node)
		if first {
			b.Seed(genuine)
		}
		for i := range 2000 {
			b.Seed(node(fmt.Sprintf("fake-%d", i), "uuid=peer"))
		}
		b.Seed(genuine)
		if len(b.Nodes()) != 8 {
			t.Fatalf("same-UUID bound: %d", len(b.Nodes()))
		}
		if _, ok := b.nodes[b.key(genuine)]; !ok {
			t.Fatal("flood crowded out previously verified endpoint")
		}
		for i := range 2000 {
			b.Seed(node(fmt.Sprintf("other-%d", i), fmt.Sprintf("uuid=other-%d", i)))
		}
		if len(b.Nodes()) != 256 || len(b.misses) > 256 {
			t.Fatalf("global bound: %d nodes, %d counters", len(b.nodes), len(b.misses))
		}
		if _, ok := b.nodes[b.key(genuine)]; !ok {
			t.Fatal("global flood displaced preferred endpoint")
		}
	}
}

func TestCandidateSamplingDoesNotPermanentlyExcludeNewPeer(t *testing.T) {
	b := candidateBrowser(WithCandidateLimits(8, 256, nil))
	genuine := node("genuine", "uuid=peer")
	admitted := false
	for round := uint64(1); round <= 128; round++ {
		b.candidateSalt = round
		seen := make(map[string]Node)
		for i := range 64 {
			b.observeCandidate(seen, node(fmt.Sprintf("fake-%d", i), "uuid=peer"))
		}
		b.observeCandidate(seen, genuine)
		b.reconcile(seen)
		admitted = admitted || b.nodes[b.key(genuine)].ID == genuine.ID
		if len(b.nodes) > 8 {
			t.Fatalf("retained %d", len(b.nodes))
		}
	}
	if !admitted {
		t.Fatal("fixed flood permanently excluded new candidate")
	}
}
