// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"github.com/miekg/dns"
	"net"
	"testing"
)

func TestPacketDecoderPreservesIPv6AndDNSNameCaseRules(t *testing.T) {
	b := New("_pair-test._tcp", "local")
	packet := &dns.Msg{MsgHdr: dns.MsgHdr{Response: true}, Answer: []dns.RR{
		&dns.SRV{Hdr: dns.RR_Header{Name: "Peer._PAIR-TEST._TCP.LOCAL.", Rrtype: dns.TypeSRV, Ttl: 120}, Target: "HOST.LOCAL.", Port: 9},
	}, Extra: []dns.RR{
		&dns.TXT{Hdr: dns.RR_Header{Name: "peer._pair-test._tcp.local.", Rrtype: dns.TypeTXT, Ttl: 120}, Txt: []string{"uuid=test"}},
		&dns.AAAA{Hdr: dns.RR_Header{Name: "host.local.", Rrtype: dns.TypeAAAA, Ttl: 120}, AAAA: net.ParseIP("2001:db8::1")},
		&dns.AAAA{Hdr: dns.RR_Header{Name: "host.local.", Rrtype: dns.TypeAAAA, Ttl: 120}, AAAA: net.ParseIP("fe80::1")},
		&dns.A{Hdr: dns.RR_Header{Name: "host.local.", Rrtype: dns.TypeA, Ttl: 0}, A: net.ParseIP("192.0.2.1")},
	}}
	got := b.nodesFromPacket(packet)
	if len(got) != 1 || got[0].ID != "Peer" || len(got[0].TXT) != 1 || got[0].TXT[0] != "uuid=test" || len(got[0].Addresses) != 1 || got[0].Addresses[0] != "2001:db8::1" {
		t.Fatalf("decoded %+v", got)
	}
	packet.Response = false
	if got := b.nodesFromPacket(packet); len(got) != 0 {
		t.Fatalf("query counted as response: %v", got)
	}
	packet.Response = true
	packet.Answer[0].Header().Ttl = 0
	if got := b.nodesFromPacket(packet); len(got) != 0 {
		t.Fatalf("expired SRV counted as alive: %v", got)
	}
	packet.Answer[0].Header().Ttl = 120
	packet.Answer[0].Header().Name = "peer._unrelated._tcp.local."
	if got := b.nodesFromPacket(packet); len(got) != 0 {
		t.Fatalf("unrelated service accepted: %v", got)
	}
}
