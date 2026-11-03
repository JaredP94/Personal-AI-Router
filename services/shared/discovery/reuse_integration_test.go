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
	"nvpair-shared/mdns"
	"nvpair-shared/netmon"
)

func TestRepeatedScansSeeUpdatesAndThenMisses(t *testing.T) {
	requireMulticast(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	responderCtx, stopResponder := context.WithCancel(ctx)
	defer stopResponder()
	r, err := mdns.NewResponder("reuse-test", "_pair-reuse-test._tcp", "local", 9, []string{"phase=1"})
	if err != nil {
		t.Fatal(err)
	}
	responderDone := make(chan error, 1)
	go func() { responderDone <- r.Run(responderCtx) }()
	b := New("_pair-reuse-test._tcp", "local", WithInterval(100*time.Millisecond), WithScanTimeout(50*time.Millisecond), WithMissThreshold(3))
	events := make(chan Event, 16)
	browserDone := make(chan struct{})
	go func() { defer close(browserDone); b.Run(ctx, events) }()
	defer func() {
		cancel()
		<-browserDone
		stopResponder()
		if err := <-responderDone; err != nil {
			t.Errorf("responder: %v", err)
		}
	}()
	next := func(kind, txt string) {
		t.Helper()
		for {
			select {
			case event, ok := <-events:
				if !ok {
					t.Fatalf("browser stopped before %s", kind)
				}
				if event.Node.ID == "reuse-test" && event.Type == kind {
					if txt != "" && (len(event.Node.TXT) != 1 || event.Node.TXT[0] != txt) {
						t.Fatalf("%s has stale TXT: %v", kind, event.Node.TXT)
					}
					return
				}
			case <-ctx.Done():
				t.Fatalf("no %s event: %v", kind, ctx.Err())
			}
		}
	}
	next(Discovered, "phase=1")
	r.UpdateTXT([]string{"phase=2"})
	next(Updated, "phase=2")
	stopResponder()
	next(Removed, "")
}

func TestQueryUsesSnapshotAndPreservesPerInterfaceOutcomes(t *testing.T) {
	snapshot := netmon.Snapshot{Interfaces: map[int]net.Interface{
		7: {Index: 7, Name: "eth0", Flags: net.FlagUp | net.FlagMulticast},
		8: {Index: 8, Name: "eth1", Flags: net.FlagUp | net.FlagMulticast},
	}, IfaceV4: map[int][]net.IP{7: {net.ParseIP("192.0.2.1")}, 8: {net.ParseIP("192.0.2.2")}}}
	calls := make(map[string]int)
	send := func(buf []byte, ifi *net.Interface, ip net.IP, target *net.UDPAddr) error {
		calls[ifi.Name]++
		var query dns.Msg
		if err := query.Unpack(buf); err != nil {
			t.Fatal(err)
		}
		if len(query.Question) != 1 || query.Question[0].Name != "_pair-test._tcp.local." || target.Port != 5353 || !target.IP.Equal(net.IPv4(224, 0, 0, 251)) {
			t.Fatal("invalid mDNS query/target")
		}
		if ifi.Name == "eth0" {
			return errors.New("send refused")
		}
		return nil
	}
	ipv6Calls := 0
	got := sendMulticastQuery("_pair-test._tcp", "local", snapshot, send, func([]byte) { ipv6Calls++ })
	if len(got) != 2 || got["eth0"] || !got["eth1"] || calls["eth0"] != 1 || calls["eth1"] != 1 || ipv6Calls != 1 {
		t.Fatalf("send outcomes=%v calls=%v ipv6=%d", got, calls, ipv6Calls)
	}
}

func TestReceiverFailureDoesNotSuppressSendEvidence(t *testing.T) {
	n := &browserNetwork{snapshot: netmon.Snapshot{
		Interfaces: map[int]net.Interface{7: {Index: 7, Name: "test-interface", Flags: net.FlagUp | net.FlagMulticast}},
		IfaceV4:    map[int][]net.IP{7: {net.ParseIP("192.0.2.123")}},
	}}
	defer n.sender.Close()
	b := New("_pair-test._tcp", "local")
	for i := 0; i < 3; i++ {
		b.recordSendOutcomes(n.query(b.service, b.domain))
	}
	if !b.SendFailures()["test-interface"] {
		t.Fatal("receiver failure suppressed failed source-bound writes")
	}
}

func TestSendFailuresClearWhenAllInterfacesDisappear(t *testing.T) {
	b := New("_pair-test._tcp", "local")
	for i := 0; i < 3; i++ {
		b.recordSendOutcomes(map[string]bool{"eth0": false})
	}
	b.recordSendOutcomes(nil)
	if len(b.SendFailures()) != 0 {
		t.Fatal("removed interfaces retained stale failure evidence")
	}
}
