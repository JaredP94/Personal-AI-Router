// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mdns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// testSenderListener checks the production source-port contract before binding
// a real socket on an isolated port. With port zero, the first bind selects an
// ephemeral port; later binds (including another owner) share that endpoint.
func testSenderListener(port int) func(context.Context, string, string) (net.PacketConn, error) {
	var mu sync.Mutex
	return func(ctx context.Context, network, address string) (net.PacketConn, error) {
		mu.Lock()
		defer mu.Unlock()
		addr, err := net.ResolveUDPAddr(network, address)
		if err != nil {
			return nil, err
		}
		if addr.Port != 5353 {
			return nil, fmt.Errorf("sender requested source port %d, want 5353", addr.Port)
		}
		addr.Port = port
		lc := net.ListenConfig{Control: setSenderReuse}
		conn, err := lc.ListenPacket(ctx, network, addr.String())
		if err == nil {
			port = conn.LocalAddr().(*net.UDPAddr).Port
		}
		return conn, err
	}
}

func TestSenderReportsOccupiedSourcePortAndRetries(t *testing.T) {
	ifi, source := loopbackIPv4(t)
	sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: source})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	target := sink.LocalAddr().(*net.UDPAddr)
	// Whether a held port refuses a second bind is platform-specific (Windows
	// SO_REUSEADDR binds over a non-exclusive owner), so the occupied port is
	// injected at the bind hook. The retry binds a real socket.
	var occupied atomic.Bool
	occupied.Store(true)
	bind := testSenderListener(0)
	s := Sender{listenPacket: func(ctx context.Context, network, address string) (net.PacketConn, error) {
		if occupied.Load() {
			return nil, &net.OpError{Op: "listen", Net: network, Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}
		}
		return bind(ctx, network, address)
	}}
	defer s.Close()
	if err := s.Send([]byte("blocked"), ifi, source, target, nil); !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("occupied source port: %v, want EADDRINUSE", err)
	}
	if len(s.sockets) != 0 {
		t.Fatal("failed bind cached as a working socket")
	}
	occupied.Store(false)
	if err := s.Send([]byte("retry"), ifi, source, target, nil); err != nil {
		t.Fatalf("retry after source port released: %v", err)
	}
	buf := make([]byte, 64)
	if err := sink.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	n, _, err := sink.ReadFromUDP(buf)
	if err != nil || string(buf[:n]) != "retry" {
		t.Fatalf("retried packet: %q, %v", buf[:n], err)
	}
}
