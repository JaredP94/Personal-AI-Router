// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	"nvpair-shared/netmon"
)

func TestPartialReceiverSetupRetriesWithoutNetworkChanges(t *testing.T) {
	for _, family := range []string{"udp4", "udp6"} {
		for _, failure := range []string{"bind", "one-membership", "all-memberships"} {
			t.Run(family+"/"+failure, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				now := time.Unix(1000, 0)
				broken := true
				opens := map[string]int{}
				joins := map[string]map[int]int{"udp4": {}, "udp6": {}}
				var sockets []net.PacketConn
				latest := map[string]net.PacketConn{}
				open := func(_ context.Context, network, _ string) (*receiveSocket, error) {
					opens[network]++
					if broken && network == family && failure == "bind" {
						return nil, errors.New("temporary bind failure")
					}
					conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
					if err != nil {
						return nil, err
					}
					sockets = append(sockets, conn)
					latest[network] = conn
					return &receiveSocket{PacketConn: conn, join: func(ifi *net.Interface, _ net.Addr) error {
						joins[network][ifi.Index]++
						if broken && network == family && (failure == "all-memberships" || failure == "one-membership" && ifi.Index == 2) {
							return errors.New("temporary join failure")
						}
						return nil
					}}, nil
				}
				snapshot := netmon.Snapshot{Interfaces: map[int]net.Interface{
					1: {Index: 1, Name: "test1", Flags: net.FlagUp | net.FlagMulticast},
					2: {Index: 2, Name: "test2", Flags: net.FlagUp | net.FlagMulticast},
				}}
				packets := make(chan *dns.Msg, 1)
				r, err := newResolverWithReceiver(ctx, snapshot, packets, open, func() time.Time { return now })
				if err != nil {
					t.Fatal(err)
				}
				n := &browserNetwork{ctx: ctx, cancel: cancel, resolver: r, snapshot: snapshot}
				defer func() {
					n.close()
					for _, conn := range sockets {
						if err := conn.SetReadDeadline(time.Now()); !errors.Is(err, net.ErrClosed) {
							t.Errorf("receiver leaked: %v", err)
						}
					}
				}()
				attempts := func() int {
					if failure == "bind" {
						return opens[family]
					}
					return joins[family][2]
				}
				// The scan boundary drives retries, not network events or sleeps.
				for i, delay := range []time.Duration{1, 2, 4, 8, 16, 32, 60, 60} {
					now = now.Add(delay*time.Second - time.Nanosecond)
					if err := n.prepare(); err != nil {
						t.Fatal(err)
					}
					if got := attempts(); got != i+1 {
						t.Fatalf("retried before backoff elapsed: attempts=%d, want %d", got, i+1)
					}
					now = now.Add(time.Nanosecond)
					if i == 7 {
						broken = false
					}
					if err := n.prepare(); err != nil {
						t.Fatal(err)
					}
					if got := attempts(); got != i+2 {
						t.Fatalf("failed setup not retried after backoff: attempts=%d, want %d", got, i+2)
					}
				}
				if n.resolver != r || len(r.conns) != 2 {
					t.Fatalf("healthy resolver replaced or missing recovered family: connections=%d", len(r.conns))
				}
				for network := range opens {
					if network != family || failure == "one-membership" {
						if opens[network] != 1 || joins[network][1] != 1 {
							t.Errorf("successful socket/membership retried: %s opens=%d joins=%v", network, opens[network], joins[network])
						}
					}
				}
				now = now.Add(time.Hour)
				if err := n.prepare(); err != nil {
					t.Fatal(err)
				}
				if got := attempts(); got != 9 {
					t.Fatalf("setup retried after recovery: %d attempts", got)
				}
				// A recovered family must actually start its packet reader.
				writer, err := net.Dial("udp4", latest[family].LocalAddr().String())
				if err != nil {
					t.Fatal(err)
				}
				defer writer.Close()
				buf, err := (&dns.Msg{MsgHdr: dns.MsgHdr{Response: true, Id: 42}}).Pack()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := writer.Write(buf); err != nil {
					t.Fatal(err)
				}
				select {
				case msg := <-packets:
					if msg.Id != 42 {
						t.Fatalf("unexpected packet: %v", msg)
					}
				case <-time.After(time.Second):
					t.Fatal("recovered receiver did not deliver a packet")
				}
			})
		}
	}
}
