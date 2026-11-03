// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mdns

import (
	"errors"
	"net"
	"testing"
	"time"

	"nvpair-shared/netmon"
)

func TestSenderReusesSocketAndRefreshesRemovedInterface(t *testing.T) {
	ifi, source := loopbackIPv4(t)
	sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: source})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	s := Sender{listenPacket: testSenderListener(0)}
	defer s.Close()
	target := sink.LocalAddr().(*net.UDPAddr)
	var original net.PacketConn
	for i := 0; i < 3; i++ {
		if err := s.Send([]byte("packet"), ifi, source, target, nil); err != nil {
			t.Fatal(err)
		}
		for _, socket := range s.sockets {
			if original == nil {
				original = socket.conn
			} else if original != socket.conn {
				t.Fatal("repeated send replaced cached socket")
			}
		}
	}
	if len(s.sockets) != 1 {
		t.Fatalf("cached sockets=%d, want 1", len(s.sockets))
	}
	snap := netmon.Snapshot{Interfaces: map[int]net.Interface{ifi.Index: *ifi}, IfaceV4: map[int][]net.IP{ifi.Index: {source}}}
	s.Refresh(snap)
	for _, socket := range s.sockets {
		if socket.conn != original {
			t.Fatal("unchanged snapshot replaced sender")
		}
	}
	snap.IfaceV4[ifi.Index] = []net.IP{net.ParseIP("192.0.2.123")}
	s.Refresh(snap)
	if len(s.sockets) != 0 {
		t.Fatal("re-addressed interface retained stale sender")
	}
	if _, err := original.WriteTo([]byte("closed"), target); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("re-addressed sender still open: %v", err)
	}
	if err := s.Send([]byte("returned"), ifi, source, target, nil); err != nil {
		t.Fatal(err)
	}
	for _, socket := range s.sockets {
		original = socket.conn
	}
	s.Refresh(netmon.Snapshot{})
	if len(s.sockets) != 0 {
		t.Fatal("removed interface retained sender")
	}
	if _, err := original.WriteTo([]byte("closed"), target); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("retired socket still open: %v", err)
	}
	if err := s.Send([]byte("again"), ifi, source, target, nil); err != nil {
		t.Fatal(err)
	}
	for _, socket := range s.sockets {
		if socket.conn == original {
			t.Fatal("removed sender reused after interface returned")
		}
	}
	s.Close()
	if err := s.Send([]byte("closed"), ifi, source, target, nil); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("send after close: %v", err)
	}
}

func TestSenderReadsUnicastAndReportsBindFailures(t *testing.T) {
	ifi, source := loopbackIPv4(t)
	sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: source})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	sink.SetReadDeadline(time.Now().Add(time.Second))
	received := make(chan string, 1)
	s := Sender{listenPacket: testSenderListener(0)}
	defer s.Close()
	callback := func(buf []byte, index int, from net.Addr) {
		if index == ifi.Index {
			received <- string(buf)
		}
	}
	if err := s.Send([]byte("query"), ifi, source, sink.LocalAddr().(*net.UDPAddr), callback); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 32)
	_, from, err := sink.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	if from.Port == 5353 || from.Port == 0 {
		t.Fatalf("source port=%d, want an isolated ephemeral port", from.Port)
	}
	if _, err := sink.WriteToUDP([]byte("reply"), from); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if got != "reply" {
			t.Fatalf("received %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("source-bound socket swallowed unicast reply")
	}
	if err := s.Send([]byte("bad"), ifi, net.ParseIP("192.0.2.123"), sink.LocalAddr().(*net.UDPAddr), nil); err == nil {
		t.Fatal("nonlocal bind succeeded")
	}
	if len(s.sockets) != 1 {
		t.Fatal("failed bind cached as working socket")
	}
}
