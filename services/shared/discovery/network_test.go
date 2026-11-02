// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"errors"
	"github.com/miekg/dns"
	"net"
	"testing"
	"time"
)

func TestBrowserReusesResolverAcrossScans(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := New("_pair-browser-test._tcp", "local", WithScanTimeout(10*time.Millisecond))
	b.runCtx = ctx
	defer func() {
		if b.network != nil {
			b.network.close()
		}
	}()
	b.browse(ctx)
	first := b.network.resolver
	if first == nil {
		t.Fatal("no resolver")
	}
	b.browse(ctx)
	if b.network.resolver != first {
		t.Fatal("periodic scan rebuilt resolver")
	}
	b.network.close()
	for _, conn := range first.conns {
		if err := conn.SetReadDeadline(time.Now()); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("receiver not closed: %v", err)
		}
	}
}

func TestReceiverFailureRebuildsTransportWithoutAnInterfaceEvent(t *testing.T) {
	n := newBrowserNetwork(context.Background())
	defer n.close()
	if err := n.prepare(); err != nil {
		t.Fatal(err)
	}
	first := n.resolver
	if err := first.conns[0].Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-first.failed:
	case <-time.After(time.Second):
		t.Fatal("receiver did not report socket failure")
	}
	if err := n.prepare(); err != nil {
		t.Fatal(err)
	}
	if n.resolver == first {
		t.Fatal("failed receiver reused forever without a network event")
	}
}

func TestNetworkChangeRefreshesResolverAndSenders(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := newBrowserNetwork(ctx)
	defer n.close()
	if err := n.refresh(n.monitor.Snapshot()); err != nil {
		t.Fatal(err)
	}
	first := n.resolver
	same := n.monitor.Snapshot()
	if err := n.refresh(same); err != nil {
		t.Fatal(err)
	}
	if n.resolver != first {
		t.Fatal("unchanged snapshot replaced resolver")
	}
	// Simulate an interface removal using an immutable netmon snapshot, without
	// modifying the host's network configuration.
	changed := n.monitor.Snapshot()
	for index := range changed.Interfaces {
		delete(changed.Interfaces, index)
		delete(changed.IfaceV4, index)
		break
	}
	_ = n.refresh(changed)
	if n.resolver == first {
		t.Fatal("interface change retained old resolver")
	}
	for _, conn := range first.conns {
		if err := conn.SetReadDeadline(time.Now()); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("stale receiver not closed: %v", err)
		}
	}
}

func TestResponsesRemainFreshAcrossScans(t *testing.T) {
	b := New("_pair-test._tcp", "local")
	packet := func(ip string, port uint16) *dns.Msg {
		return &dns.Msg{MsgHdr: dns.MsgHdr{Response: true}, Answer: []dns.RR{
			&dns.PTR{Hdr: dns.RR_Header{Name: "_pair-test._tcp.local.", Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 120}, Ptr: "peer._pair-test._tcp.local."},
			&dns.SRV{Hdr: dns.RR_Header{Name: "peer._pair-test._tcp.local.", Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: 120}, Port: port, Target: "peer.local."},
			&dns.TXT{Hdr: dns.RR_Header{Name: "peer._pair-test._tcp.local.", Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 120}, Txt: []string{"uuid=peer"}},
			&dns.A{Hdr: dns.RR_Header{Name: "peer.local.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 120}, A: net.ParseIP(ip)},
			&dns.AAAA{Hdr: dns.RR_Header{Name: "peer.local.", Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 120}, AAAA: net.ParseIP("2001:db8::1")},
		}}
	}
	for _, tc := range []struct {
		ip   string
		port uint16
	}{{"192.0.2.1", 9}, {"192.0.2.2", 10}} {
		nodes := b.nodesFromPacket(packet(tc.ip, tc.port))
		if len(nodes) != 1 || nodes[0].ID != "peer" || nodes[0].Port != int(tc.port) || len(nodes[0].Addresses) != 2 || nodes[0].Addresses[0] != tc.ip {
			t.Fatalf("stale/incomplete result: %+v", nodes)
		}
	}
	b.opt.selfInstance = "peer"
	if got := b.nodesFromPacket(packet("192.0.2.1", 9)); len(got) != 0 {
		t.Fatalf("self not filtered: %v", got)
	}
}
