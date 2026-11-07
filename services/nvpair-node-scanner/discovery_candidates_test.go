// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"nvpair-shared/noderec"
)

func TestBrowserDeliversSameUUIDCandidatesInBothOrders(t *testing.T) {
	for _, tc := range []struct{ reverse, sameName bool }{{}, {reverse: true}, {sameName: true}, {reverse: true, sameName: true}} {
		reverse := tc.reverse
		name := "genuine-first"
		if reverse {
			name = "lookalike-first"
		}
		if tc.sameName {
			name += "-same-instance"
		}
		t.Run(name, func(t *testing.T) {
			d, srv, _ := secureInventoryFixture(t, "uuid-peer")
			host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
			public := httptest.NewServer(http.NotFoundHandler())
			defer public.Close()
			ph, pp, _ := net.SplitHostPort(public.Listener.Addr().String())
			genuine := RawNode{ID: "genuine", Host: "genuine.local.", Port: 14318, Addresses: []string{host}, TXT: []string{"uuid=uuid-peer", "cluster-uuid=uuid-peer", "ip=" + host, "ni=" + port}}
			lookalike := RawNode{ID: "lookalike", Host: "lookalike.local.", Port: 14318, Addresses: []string{ph}, TXT: []string{"uuid=uuid-peer", "ip=" + ph, "ni=" + pp}}
			if tc.sameName {
				// Reusing an instance on a distinct SRV host is an independent
				// advertiser, not an obsolete revision of the genuine one.
				// Copying both is covered by TestLookalikeAdvertiserCannotRemoveVerifiedPeer.
				lookalike.ID = genuine.ID
			}
			browser := newDaemon(nil, d.mesh, d.clusterDir, nil).browser
			inputs := []RawNode{genuine, lookalike}
			if reverse {
				inputs[0], inputs[1] = inputs[1], inputs[0]
			}
			browser.Seed(inputs...)
			if len(browser.Nodes()) != 2 {
				t.Fatalf("browser retained %d candidates, want both", len(browser.Nodes()))
			}
			for _, raw := range browser.Nodes() {
				d.onBrowse(DiscoveryEvent{Type: "discovered", Node: raw})
			}
			verified, ok := d.dir.get("uuid-peer")
			if !ok || !verified.ServicePeer() {
				t.Fatal("genuine candidate not verified")
			}
			preview, _ := toDirectoryNode(lookalike)
			stored, ok := d.dir.get(preview.RecordKey())
			if !ok || stored.Trusted || stored.IdentityVerification != "unverified" {
				t.Fatalf("lookalike not independently previewed: %+v", stored)
			}
			d.onBrowse(DiscoveryEvent{Type: "removed", Node: lookalike})
			if _, ok := d.dir.get(preview.RecordKey()); ok {
				t.Fatal("preview not removed")
			}
			if peer, ok := d.dir.get("uuid-peer"); !ok || !peer.ServicePeer() {
				t.Fatal("preview removal evicted proven peer")
			}
			if len(d.dir.snapshot(noderec.ServiceNodeInfo)) != 1 {
				t.Fatal("unexpected node-info records after preview removal")
			}
		})
	}
}
