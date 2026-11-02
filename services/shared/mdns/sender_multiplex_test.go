// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mdns

import (
	"net"
	"testing"
	"time"
)

// Browser and responder senders share the same source endpoint. The OS delivers
// a unicast datagram to only one socket, so that reader must fan it out to both
// owners instead of swallowing the other owner's questions or responses.
func TestSendersMultiplexUnicastAcrossOwners(t *testing.T) {
	ifi, source := loopbackIPv4(t)
	sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: source})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	sink.SetReadDeadline(time.Now().Add(time.Second))
	var browser, responder Sender
	defer browser.Close()
	defer responder.Close()
	browserPackets, responderPackets := make(chan string, 4), make(chan string, 4)
	for _, owner := range []struct {
		sender  *Sender
		packets chan string
	}{{&browser, browserPackets}, {&responder, responderPackets}} {
		handler := func(buf []byte, _ int, _ net.Addr) { owner.packets <- string(buf) }
		if err := owner.sender.Send([]byte("startup"), ifi, source, sink.LocalAddr().(*net.UDPAddr), handler); err != nil {
			t.Fatal(err)
		}
	}
	buf := make([]byte, 64)
	var from *net.UDPAddr
	for i := 0; i < 2; i++ {
		_, from, err = sink.ReadFromUDP(buf)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sink.WriteToUDP([]byte("unicast"), from); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []struct {
		name    string
		packets chan string
	}{{"browser", browserPackets}, {"responder", responderPackets}} {
		select {
		case got := <-owner.packets:
			if got != "unicast" {
				t.Fatalf("%s received %q", owner.name, got)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s lost unicast traffic to the other sender", owner.name)
		}
	}
	// Closing one owner must neither disable nor retain callbacks for that owner.
	browser.Close()
	if _, err := sink.WriteToUDP([]byte("after-close"), from); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-responderPackets:
		if got != "after-close" {
			t.Fatalf("remaining owner received %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("closing other owner stopped surviving receiver")
	}
	select {
	case got := <-browserPackets:
		t.Fatalf("closed owner received %q", got)
	default:
	}
}
