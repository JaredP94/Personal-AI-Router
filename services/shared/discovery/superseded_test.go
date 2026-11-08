// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"slices"
	"testing"
)

func TestSupersededAdvertisementCannotBorrowEndpointLiveness(t *testing.T) {
	for _, change := range []string{"txt", "address", "port"} {
		t.Run(change, func(t *testing.T) {
			probes := 0
			b := candidateBrowser(WithLivenessProbe(func(Node) bool { probes++; return true }))
			old := Node{ID: "Peer", Host: "Peer.local.", Port: 14318, Addresses: []string{"192.0.2.1"}, TXT: []string{"uuid=peer", "ni=12345"}}
			b.Seed(old)
			next := old
			next.ID, next.Host = "peer", "peer.LOCAL."
			switch change {
			case "txt":
				next.TXT = append(slices.Clone(old.TXT), "cluster-uuid=peer")
			case "address":
				next.Addresses = []string{"192.0.2.2"}
			case "port":
				next.Port++
			}
			events := b.reconcile(map[string]Node{b.key(next): next})
			if got := b.Nodes(); len(got) != 1 || b.key(got[0]) != b.key(next) {
				t.Fatalf("superseded record remains: %+v", got)
			}
			removed := false
			for _, event := range events {
				removed = removed || event.Type == Removed && b.key(event.Node) == b.key(old)
			}
			if !removed || probes != 0 {
				t.Fatalf("retirement removed=%v probes=%d", removed, probes)
			}
		})
	}
}

func TestAccumulateOnlyBrowserRetainsAdvertisementHistory(t *testing.T) {
	b := candidateBrowser(WithNoEviction())
	old := Node{ID: "peer", Host: "peer.local.", TXT: []string{"uuid=peer"}}
	b.Seed(old)
	next := old
	next.TXT = []string{"uuid=peer", "cluster-uuid=peer"}
	b.reconcile(map[string]Node{b.key(next): next})
	if len(b.Nodes()) != 2 {
		t.Fatal("advertiser retirement overrode explicit accumulate-only policy")
	}
}

func TestConcurrentAdvertiserVariantsRemainAvailableForAuthentication(t *testing.T) {
	b := candidateBrowser()
	genuine := Node{ID: "peer", Host: "peer.local.", TXT: []string{"uuid=peer", "ip=192.0.2.1"}}
	fake := genuine
	fake.TXT = []string{"uuid=peer", "ip=192.0.2.2"}
	b.reconcile(map[string]Node{b.key(genuine): genuine, b.key(fake): fake})
	if len(b.Nodes()) != 2 {
		t.Fatal("concurrent copied-name variant hid genuine candidate")
	}
	other := genuine
	other.Host = "other.local."
	b.reconcile(map[string]Node{b.key(other): other})
	if len(b.Nodes()) != 3 {
		t.Fatal("different host superseded unrelated candidates")
	}
}
