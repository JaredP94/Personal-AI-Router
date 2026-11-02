// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mdns

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"nvpair-shared/netmon"

	"golang.org/x/net/ipv4"
)

const (
	mdnsPort              = 5353
	preferredMulticastTTL = 255
	fallbackMulticastTTL  = 1
)

type packetWriter interface {
	WriteTo([]byte, net.Addr) (int, error)
}

type multicastOptions interface {
	SetMulticastInterface(*net.Interface) error
	SetMulticastTTL(int) error
}

// Sender owns reusable, source-bound UDP 5353 sockets. Its zero value is ready
// to use. The owner must call Refresh on interface changes and Close at shutdown.
// Source binding preserves the Windows multicast-send workaround on every send.
// Methods are safe to call concurrently.
type Sender struct {
	mu        sync.Mutex
	sockets   map[senderKey]*senderSocket
	closed    bool
	readers   sync.WaitGroup
	callbacks sync.WaitGroup
}

type senderKey struct {
	index int
	name  string
	ip    string
}

type senderSocket struct {
	conn                net.PacketConn
	options             *ipv4.PacketConn
	multicastConfigured bool
}

// Send transmits one packet and returns the actual bind/write outcome, including
// on a reused socket. receive handles packets delivered to the source-bound
// socket (notably unicast replies/questions); it must not block. The callback
// is fixed by the first send on that socket and must not call Close.
func (s *Sender) Send(buf []byte, ifi *net.Interface, src net.IP, target *net.UDPAddr, receive func([]byte, int, net.Addr)) error {
	source := src.To4()
	if source == nil {
		return errors.New("mDNS source is not IPv4")
	}
	if target == nil || target.IP.To4() == nil || target.Port == 0 {
		return errors.New("mDNS target is not a valid IPv4 endpoint")
	}

	if ifi == nil {
		return errors.New("mDNS sender requires an interface")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return net.ErrClosed
	}
	key := senderKey{index: ifi.Index, name: ifi.Name, ip: source.String()}
	socket := s.sockets[key]
	if socket == nil {
		lc := net.ListenConfig{Control: setSenderReuse}
		conn, err := lc.ListenPacket(context.Background(), "udp4", (&net.UDPAddr{IP: source, Port: mdnsPort}).String())
		if err != nil {
			return fmt.Errorf("bind mDNS sender: %w", err)
		}
		socket = &senderSocket{conn: conn, options: ipv4.NewPacketConn(conn)}
		if s.sockets == nil {
			s.sockets = make(map[senderKey]*senderSocket)
		}
		s.sockets[key] = socket
		s.registerReceiver(key, receive)
		s.readers.Add(1)
		go func() {
			defer s.readers.Done()
			// A terminal read error must not leave a bound, unreadable socket
			// swallowing traffic. The next write reports closure and retires it.
			defer conn.Close()
			buf := make([]byte, 65536)
			for {
				n, from, err := conn.ReadFrom(buf)
				if err != nil {
					return
				}
				dispatchSenderPacket(key, buf[:n], from)
			}
		}()
	}
	err := writePacket(buf, ifi, source, target, socket.conn, socket.options, &socket.multicastConfigured)
	if err != nil {
		socket.multicastConfigured = false
	}
	if errors.Is(err, net.ErrClosed) {
		_ = socket.conn.Close()
		s.unregisterReceiver(key)
		delete(s.sockets, key)
	}
	return err
}

// Refresh retires sockets whose selected interface/address disappeared or
// changed. Unchanged sockets survive; new ones are bound lazily by Send so bind
// failures continue to be reported and retried on each scan.
func (s *Sender) Refresh(snapshot netmon.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, socket := range s.sockets {
		ifi, ok := snapshot.Interfaces[key.index]
		ips := snapshot.IfaceV4[key.index]
		if ok && ifi.Name == key.name && len(ips) > 0 && ips[0].String() == key.ip {
			continue
		}
		_ = socket.conn.Close()
		s.unregisterReceiver(key)
		delete(s.sockets, key)
	}
}

// Close releases all sockets and waits for their receive loops to exit.
func (s *Sender) Close() {
	s.mu.Lock()
	s.closed = true
	for key, socket := range s.sockets {
		_ = socket.conn.Close()
		s.unregisterReceiver(key)
		delete(s.sockets, key)
	}
	s.mu.Unlock()
	s.readers.Wait()
	s.callbacks.Wait()
}

// ListenPacket opens a shared mDNS receive socket on UDP 5353.
func ListenPacket(ctx context.Context, network, address string) (net.PacketConn, error) {
	lc := net.ListenConfig{Control: setReuseAddr}
	return lc.ListenPacket(ctx, network, address)
}

func writePacket(
	buf []byte,
	ifi *net.Interface,
	source net.IP,
	target *net.UDPAddr,
	conn packetWriter,
	options multicastOptions,
	multicastConfigured *bool,
) error {
	if target.IP.IsMulticast() && !*multicastConfigured {
		configured := true
		if ifi == nil {
			return errors.New("mDNS multicast target requires an interface")
		}
		if err := options.SetMulticastInterface(ifi); err != nil {
			configured = false
			slog.Debug("mdns: set multicast interface failed; sending with socket route",
				"iface", ifi.Name,
				"ip", source.String(),
				"target", target.String(),
				"err", err)
		}
		if err := options.SetMulticastTTL(preferredMulticastTTL); err != nil {
			configured = false
			slog.Debug("mdns: set multicast TTL failed; retrying with TTL 1",
				"iface", ifi.Name,
				"ip", source.String(),
				"target", target.String(),
				"ttl", preferredMulticastTTL,
				"err", err)
			if fallbackErr := options.SetMulticastTTL(fallbackMulticastTTL); fallbackErr != nil {
				slog.Debug("mdns: set multicast fallback TTL failed; sending with socket default",
					"iface", ifi.Name,
					"ip", source.String(),
					"target", target.String(),
					"ttl", fallbackMulticastTTL,
					"err", fallbackErr)
			}
		}
		// Multicast interface selection may query addresses inside x/net.
		// Cache successful configuration too, not just the bound descriptor.
		// Advisory failures are retried on the next send, as before.
		*multicastConfigured = configured
	}

	if _, err := conn.WriteTo(buf, target); err != nil {
		return fmt.Errorf("write mDNS packet: %w", err)
	}
	return nil
}
