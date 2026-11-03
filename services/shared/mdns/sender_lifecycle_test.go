// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mdns

import (
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// A persistent source-bound socket must consume unicast questions itself:
// otherwise it steals them from the multicast-bound receiver on Linux.
func TestResponderKeepsSourceSocketForUnicastQuestions(t *testing.T) {
	ifi, source := loopbackIPv4(t)
	sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: source})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	sink.SetReadDeadline(time.Now().Add(time.Second))
	r := &Responder{ifaceInfo: map[int]net.Interface{ifi.Index: *ifi}, serviceName: "_pair-sender-test._tcp.local.", instanceName: "test._pair-sender-test._tcp.local.", hostName: "test.local.", port: 9, ifaceAddrs: map[int][]net.IP{ifi.Index: {source}}}
	r.sender.listenPacket = testSenderListener(0)
	defer r.sender.Close()
	target := sink.LocalAddr().(*net.UDPAddr)
	if err := r.sendOnInterface([]byte("initial"), ifi.Index, target); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2048)
	_, from, err := sink.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	sourcePort := from.Port
	if sourcePort == 5353 || sourcePort == 0 {
		t.Fatalf("source port=%d, want an isolated ephemeral port", sourcePort)
	}
	q := new(dns.Msg)
	q.Question = []dns.Question{{Name: "_pair-sender-test._tcp.local.", Qtype: dns.TypePTR, Qclass: dns.ClassINET | cacheFlush}}
	wire, err := q.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sink.WriteToUDP(wire, from); err != nil {
		t.Fatal(err)
	}
	n, from, err := sink.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("source socket did not answer unicast question: %v", err)
	}
	var response dns.Msg
	if err = response.Unpack(buf[:n]); err != nil {
		t.Fatal(err)
	}
	if !response.Response || len(response.Answer) != 1 || from.Port != sourcePort {
		t.Fatalf("unexpected response: %v from %v", response, from)
	}
}
