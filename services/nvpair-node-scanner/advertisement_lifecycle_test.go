// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestPairingAdvertisementRetiresPreview(t *testing.T) {
	d, srv, _ := secureInventoryFixture(t, "uuid-peer")
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	raw := RawNode{ID: "Peer", Host: "peer.local.", Port: 14318, Addresses: []string{host}, TXT: []string{"uuid=uuid-peer", "ip=" + host, "ni=" + port}}
	d.onBrowse(DiscoveryEvent{Type: "discovered", Node: raw})
	oldKey := candidateRecordKey(raw)
	if _, ok := d.dir.get(oldKey); !ok {
		t.Fatal("missing pre-pairing preview")
	}
	raw.ID, raw.Host = "peer", "PEER.LOCAL."
	raw.TXT = append(slices.Clone(raw.TXT), "cluster-uuid=uuid-peer")
	d.onBrowse(DiscoveryEvent{Type: "discovered", Node: raw})
	rows := d.dir.snapshot("")
	if len(rows) != 1 || rows[0].RecordKey() != "uuid-peer" || !rows[0].ServicePeer() {
		t.Fatalf("pairing left duplicate rows: %+v", rows)
	}
	if _, ok := d.dir.get(oldKey); ok {
		t.Fatal("old preview survived pairing")
	}
	// A repeated promotion with identical authenticated inventory must clean a
	// preview too, before publishVerified's unchanged-inventory early return.
	n, _ := toDirectoryNode(raw)
	d.dir.upsert(n)
	d.candidateAttempts = nil
	d.onBrowse(DiscoveryEvent{Type: "updated", Node: raw})
	if rows := d.dir.snapshot(""); len(rows) != 1 {
		t.Fatalf("unchanged proof left preview: %+v", rows)
	}
}

func TestUnpairedAdvertisementChangesRetirePreview(t *testing.T) {
	for _, change := range []string{"service-port", "address", "srv-port"} {
		t.Run(change, func(t *testing.T) {
			d := newSelfTestDaemon("self", "127.0.0.1")
			raw := RawNode{ID: "peer", Host: "peer.local.", Port: 14318, TXT: []string{"uuid=peer", "ip=192.0.2.1"}}
			d.onBrowse(DiscoveryEvent{Type: "discovered", Node: raw})
			oldKey := candidateRecordKey(raw)
			switch change {
			case "service-port":
				raw.TXT = append(slices.Clone(raw.TXT), "ol=12345")
			case "address":
				raw.TXT = []string{"uuid=peer", "ip=192.0.2.2"}
			case "srv-port":
				raw.Port++
			}
			d.onBrowse(DiscoveryEvent{Type: "updated", Node: raw})
			if rows := d.dir.snapshot(""); len(rows) != 1 || rows[0].RecordKey() != candidateRecordKey(raw) {
				t.Fatalf("obsolete preview: %+v", rows)
			}
			if _, ok := d.dir.get(oldKey); ok {
				t.Fatal("superseded preview survived")
			}
			if len(d.candidateSnapshot()) != 1 {
				t.Fatal("superseded pending candidate survived")
			}
		})
	}
}

func TestLookalikeAdvertiserCannotRemoveVerifiedPeer(t *testing.T) {
	for _, copyName := range []string{"neither", "instance-only", "instance-and-host"} {
		t.Run(copyName, func(t *testing.T) {
			d, srv, _ := secureInventoryFixture(t, "uuid-peer")
			host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
			genuine := RawNode{ID: "genuine", Host: "genuine.local.", Port: 14318, TXT: []string{"uuid=uuid-peer", "cluster-uuid=uuid-peer", "ip=" + host, "ni=" + port}}
			fake := RawNode{ID: "lookalike", Host: "lookalike.local.", Port: 14318, TXT: []string{"uuid=uuid-peer", "ip=127.0.0.2"}}
			if copyName != "neither" {
				fake.ID = genuine.ID
			}
			if copyName == "instance-and-host" {
				fake.Host = genuine.Host
			}
			d.onBrowse(DiscoveryEvent{Type: "discovered", Node: fake})
			d.onBrowse(DiscoveryEvent{Type: "discovered", Node: genuine})
			if peer, ok := d.dir.get("uuid-peer"); !ok || !peer.ServicePeer() {
				t.Fatal("lookalike prevented real pinned-mTLS verification")
			}
			if copyName != "instance-and-host" {
				if _, ok := d.dir.get(candidateRecordKey(fake)); !ok {
					t.Fatal("verification removed unrelated lookalike")
				}
			}
			d.onBrowse(DiscoveryEvent{Type: "updated", Node: fake})
			d.onBrowse(DiscoveryEvent{Type: "removed", Node: fake})
			if peer, ok := d.dir.get("uuid-peer"); !ok || !peer.ServicePeer() {
				t.Fatal("copied advertiser withdrew verified peer")
			}
		})
	}
}

func TestVerifiedAdvertisementAddressChangeKeepsHostRecord(t *testing.T) {
	d, srv, _ := secureInventoryFixture(t, "uuid-peer")
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	raw := RawNode{ID: "peer", Host: "peer.local.", TXT: []string{"uuid=uuid-peer", "cluster-uuid=uuid-peer", "ip=" + host, "ni=" + port}}
	d.onBrowse(DiscoveryEvent{Type: "discovered", Node: raw})
	before, ok := d.dir.get("uuid-peer")
	if !ok || !before.ServicePeer() {
		t.Fatal("initial authentication failed")
	}
	// The same pinned leaf moves to another answering address and service port;
	// no old route is advertised, as in DHCP/Tailscale roaming.
	listener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	moved := httptest.NewUnstartedServer(srv.Config.Handler)
	_ = moved.Listener.Close()
	moved.Listener = listener
	moved.TLS = srv.TLS.Clone()
	moved.StartTLS()
	t.Cleanup(moved.Close)
	newHost, newPort, _ := net.SplitHostPort(listener.Addr().String())
	raw.TXT = []string{"uuid=uuid-peer", "cluster-uuid=uuid-peer", "ip=" + newHost, "ni=" + newPort}
	d.onBrowse(DiscoveryEvent{Type: "updated", Node: raw})
	rows := d.dir.snapshot("")
	if len(rows) != 1 || rows[0].RecordKey() != before.RecordKey() || rows[0].PeerRecordID != before.PeerRecordID || rows[0].IP != newHost {
		t.Fatalf("address change split proven peer: %+v", rows)
	}
}
