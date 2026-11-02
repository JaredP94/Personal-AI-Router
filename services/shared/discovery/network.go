// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"reflect"
	"strings"
	"sync"
	"time"

	"nvpair-shared/mdns"
	"nvpair-shared/netmon"

	"github.com/miekg/dns"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// A Run owns one network session. Poll without Run owns a one-shot session so
// a caller passing context.Background cannot leak sockets or monitor goroutines.
type browserNetwork struct {
	monitor  *netmon.Monitor
	changes  <-chan struct{}
	snapshot netmon.Snapshot
	resolver *resolver
	packets  chan *dns.Msg
	sender   mdns.Sender
	cancel   context.CancelFunc
	ctx      context.Context
}

func newBrowserNetwork(ctx context.Context) *browserNetwork {
	ctx, cancel := context.WithCancel(ctx)
	// Watch currently always succeeds; its platform backend owns registration
	// failures and its documented polling fallback.
	monitor, _ := netmon.Watch(ctx)
	return &browserNetwork{monitor: monitor, changes: monitor.Subscribe(), cancel: cancel, ctx: ctx, packets: make(chan *dns.Msg, 128)}
}

func (n *browserNetwork) refresh(snapshot netmon.Snapshot) error {
	if n.resolver != nil && n.resolver.healthy() && reflect.DeepEqual(n.snapshot, snapshot) {
		n.resolver.retrySetup(n.ctx)
		return nil
	}
	if n.resolver != nil {
		n.resolver.close()
		n.resolver = nil
	}
	n.sender.Refresh(snapshot)
	n.snapshot = snapshot
	var err error
	n.resolver, err = newResolver(n.ctx, snapshot, n.packets)
	return err
}

func (n *browserNetwork) prepare() error {
	select {
	case <-n.changes:
		return n.refresh(n.monitor.Snapshot())
	default:
		if n.resolver == nil || !n.resolver.healthy() {
			return n.refresh(n.monitor.Snapshot())
		}
		n.resolver.retrySetup(n.ctx)
		return nil
	}
}

func (n *browserNetwork) query(service, domain string) map[string]bool {
	send := func(buf []byte, ifi *net.Interface, src net.IP, target *net.UDPAddr) error {
		return n.sender.Send(buf, ifi, src, target, n.receive)
	}
	sendIPv6 := func(buf []byte) {
		if n.resolver != nil {
			n.resolver.sendIPv6(buf)
		}
	}
	return sendMulticastQuery(service, domain, n.snapshot, send, sendIPv6)
}

func (n *browserNetwork) close() {
	n.cancel()
	n.sender.Close()
	if n.resolver != nil {
		n.resolver.close()
	}
}

// resolver keeps receive sockets and multicast memberships alive across scans.
// It intentionally has no lifetime-wide entry deduplication: each scan must
// observe fresh answers for the existing miss/eviction state machine.
type resolver struct {
	conns       []net.PacketConn
	v6          *ipv6.PacketConn
	interfaces  []net.Interface
	packets     chan *dns.Msg
	readers     sync.WaitGroup
	failed      chan struct{}
	failureOnce sync.Once

	// Setup retries run only on the serialized scan path. Readers never access
	// these fields; successful sockets and memberships survive partial failures.
	families   []receiveFamily
	open       func(context.Context, string, string) (*receiveSocket, error)
	now        func() time.Time
	retryAt    time.Time
	retryDelay time.Duration
}

type receiveFamily struct {
	network, address, group string
	socket                  *receiveSocket
	pending                 []net.Interface
}

// receiveSocket keeps the platform multicast operations alongside their socket.
type receiveSocket struct {
	net.PacketConn
	join func(*net.Interface, net.Addr) error
	v6   *ipv6.PacketConn
}

func openReceiveSocket(ctx context.Context, network, address string) (*receiveSocket, error) {
	conn, err := mdns.ListenPacket(ctx, network, address)
	if err != nil {
		return nil, err
	}
	socket := &receiveSocket{PacketConn: conn}
	if network == "udp4" {
		socket.join = ipv4.NewPacketConn(conn).JoinGroup
	} else {
		socket.v6 = ipv6.NewPacketConn(conn)
		socket.join = socket.v6.JoinGroup
	}
	return socket, nil
}

func newResolver(ctx context.Context, snapshot netmon.Snapshot, packets chan *dns.Msg) (*resolver, error) {
	return newResolverWithReceiver(ctx, snapshot, packets, openReceiveSocket, time.Now)
}

func newResolverWithReceiver(ctx context.Context, snapshot netmon.Snapshot, packets chan *dns.Msg, open func(context.Context, string, string) (*receiveSocket, error), now func() time.Time) (*resolver, error) {
	r := &resolver{packets: packets, failed: make(chan struct{}), open: open, now: now}
	for _, ifi := range snapshot.Interfaces {
		if ifi.Flags&net.FlagUp != 0 && ifi.Flags&net.FlagMulticast != 0 {
			r.interfaces = append(r.interfaces, ifi)
		}
	}
	if len(r.interfaces) == 0 {
		return nil, errors.New("no multicast interfaces")
	}
	r.families = []receiveFamily{
		{network: "udp4", address: "224.0.0.251:5353", group: "224.0.0.251", pending: r.interfaces},
		{network: "udp6", address: "[ff02::fb]:5353", group: "ff02::fb", pending: r.interfaces},
	}
	r.retrySetup(ctx)
	if len(r.conns) == 0 {
		return nil, errors.New("failed to join any mDNS receive group")
	}
	return r, nil
}

// Retry only failed binds/memberships with 1s -> 60s capped backoff, at scan
// boundaries. An unavailable family must not churn the working receive path.
func (r *resolver) retrySetup(ctx context.Context) {
	if ctx.Err() != nil || r.now().Before(r.retryAt) {
		return
	}
	incomplete := false
	for i := range r.families {
		family := &r.families[i]
		if len(family.pending) == 0 {
			continue
		}
		fresh := family.socket == nil
		if fresh {
			var err error
			family.socket, err = r.open(ctx, family.network, family.address)
			if err != nil {
				slog.Debug("mdns: receive socket unavailable", "network", family.network, "err", err)
				incomplete = true
				continue
			}
		}
		group := &net.UDPAddr{IP: net.ParseIP(family.group)}
		var pending []net.Interface
		joined := 0
		for _, ifi := range family.pending {
			if err := family.socket.join(&ifi, group); err != nil {
				pending = append(pending, ifi)
			} else {
				joined++
			}
		}
		family.pending = pending
		incomplete = incomplete || len(pending) > 0
		if fresh {
			if joined == 0 {
				_ = family.socket.Close()
				family.socket = nil
				continue
			}
			r.startReceiver(family.socket)
		}
	}
	if incomplete {
		if r.retryDelay == 0 {
			r.retryDelay = time.Second
		} else {
			r.retryDelay = min(2*r.retryDelay, time.Minute)
		}
		r.retryAt = r.now().Add(r.retryDelay)
	} else {
		r.retryDelay = 0
		r.retryAt = time.Time{}
	}
}

func (r *resolver) startReceiver(conn *receiveSocket) {
	r.conns = append(r.conns, conn)
	if conn.v6 != nil {
		r.v6 = conn.v6
		_ = conn.v6.SetMulticastHopLimit(255)
	}
	r.readers.Add(1)
	go func() {
		defer r.readers.Done()
		buf := make([]byte, 65536)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				r.failureOnce.Do(func() { close(r.failed) })
				return
			}
			r.receive(buf[:n], 0, from)
		}
	}()
}

func (n *browserNetwork) receive(buf []byte, _ int, _ net.Addr) {
	queuePacket(n.packets, buf)
}

func (r *resolver) receive(buf []byte, _ int, _ net.Addr) {
	queuePacket(r.packets, buf)
}

func queuePacket(packets chan<- *dns.Msg, buf []byte) {
	msg := new(dns.Msg)
	if err := msg.Unpack(buf); err != nil || !msg.Response {
		return
	}
	select {
	case packets <- msg:
	default:
	}
}

func (r *resolver) sendIPv6(buf []byte) {
	if r.v6 == nil {
		return
	}
	for _, ifi := range r.interfaces {
		target := &net.UDPAddr{IP: net.ParseIP("ff02::fb"), Port: 5353, Zone: ifi.Name}
		if _, err := r.v6.WriteTo(buf, &ipv6.ControlMessage{IfIndex: ifi.Index}, target); err != nil {
			slog.Debug("mdns: IPv6 query send failed", "iface", ifi.Name, "err", err)
		}
	}
}

func (r *resolver) healthy() bool {
	select {
	case <-r.failed:
		return false
	default:
		return true
	}
}

func (r *resolver) close() {
	for _, conn := range r.conns {
		_ = conn.Close()
	}
	r.readers.Wait()
}

// nodesFromPacket decodes a complete DNS-SD response, including additional
// records. It mirrors the previous browser's packet-local resolution: a node
// needs a matching instance and at least one address in the same response.
func (b *Browser) nodesFromPacket(msg *dns.Msg) []Node {
	if !msg.Response {
		return nil
	}
	service := fmt.Sprintf("%s.%s.", strings.Trim(b.service, "."), strings.Trim(b.domain, "."))
	records := append(append(append([]dns.RR(nil), msg.Answer...), msg.Ns...), msg.Extra...)
	instances := make(map[string]Node)
	for _, record := range records {
		switch rr := record.(type) {
		case *dns.SRV:
			if rr.Hdr.Ttl == 0 || !dns.IsSubDomain(service, rr.Hdr.Name) || strings.EqualFold(rr.Hdr.Name, service) {
				continue
			}
			key := dns.CanonicalName(rr.Hdr.Name)
			node := instances[key]
			node.ID = strings.TrimSuffix(rr.Hdr.Name[:len(rr.Hdr.Name)-len(service)], ".")
			node.Host = rr.Target
			node.Port = int(rr.Port)
			instances[key] = node
		}
	}
	for _, record := range records {
		if rr, ok := record.(*dns.TXT); ok && rr.Hdr.Ttl > 0 {
			key := dns.CanonicalName(rr.Hdr.Name)
			if node, ok := instances[key]; ok {
				node.TXT = rr.Txt
				instances[key] = node
			}
		}
	}
	var nodes []Node
	for _, node := range instances {
		if b.opt.selfInstance != "" && node.ID == b.opt.selfInstance {
			continue
		}
		for _, record := range records {
			if record.Header().Ttl == 0 {
				continue
			}
			switch rr := record.(type) {
			case *dns.A:
				if strings.EqualFold(rr.Hdr.Name, node.Host) {
					node.Addresses = append(node.Addresses, rr.A.String())
				}
			case *dns.AAAA:
				if strings.EqualFold(rr.Hdr.Name, node.Host) && !isLinkLocal(rr.AAAA) {
					node.Addresses = append(node.Addresses, rr.AAAA.String())
				}
			}
		}
		if len(node.Addresses) > 0 {
			nodes = append(nodes, node)
		}
	}
	return nodes
}
