// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/ipv4"
)

var (
	multicastOnce   sync.Once
	multicastSkip   string
	multicastFailed error
	multicastNotes  []string
)

// requireMulticast skips a test that needs real mDNS on the network when this
// host cannot send IPv4 multicast through any interface. GitHub's hosted macOS
// runners are such a host: every send to 224.0.0.251 fails with "no route to
// host". It mirrors services/tests' probe (a separate module). Like discovery, the probe tries every
// eligible interface, so a host that can multicast on any of them runs the
// test. An unexpected socket error fails the test rather than skipping it.
func requireMulticast(t *testing.T) {
	t.Helper()
	multicastOnce.Do(func() {
		multicastSkip, multicastFailed, multicastNotes = decideMulticast(probeInterfaces())
	})
	for _, note := range multicastNotes {
		t.Logf("multicast probe: %s", note)
	}
	if multicastFailed != nil {
		t.Fatalf("multicast probe: %v", multicastFailed)
	}
	if multicastSkip != "" {
		t.Skipf("environment: this host cannot send mDNS multicast (%s); this test needs real mDNS discovery", multicastSkip)
	}
}

// interfaceProbe is one eligible interface's send result.
type interfaceProbe struct {
	name string
	err  error
}

// probeInterfaces sends one mDNS PTR query, for a name nothing answers, out of
// every up, multicast-capable, non-loopback interface with an IPv4 address —
// the interfaces discovery sends on.
func probeInterfaces() []interfaceProbe {
	ifaces, err := net.Interfaces()
	if err != nil {
		return []interfaceProbe{{name: "interfaces", err: err}}
	}
	var out []interfaceProbe
	for i := range ifaces {
		ifi := ifaces[i]
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		src := firstIPv4(&ifi)
		if src == nil {
			continue
		}
		out = append(out, interfaceProbe{name: ifi.Name, err: sendProbe(&ifi, src)})
	}
	return out
}

func firstIPv4(ifi *net.Interface) net.IP {
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			if ip4 := ipnet.IP.To4(); ip4 != nil {
				return ip4
			}
		}
	}
	return nil
}

func sendProbe(ifi *net.Interface, src net.IP) error {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: src})
	if err != nil {
		return err
	}
	defer conn.Close()
	pc := ipv4.NewPacketConn(conn)
	if err := pc.SetMulticastInterface(ifi); err != nil {
		return err
	}
	_ = pc.SetMulticastTTL(255)
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err = conn.WriteToUDP(mdnsProbeQuery(), &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353})
	return err
}

// decideMulticast turns the probe results into a decision. Every result is
// inspected: an unexpected error on any interface is reported as a note even
// when another interface sent, so nothing is masked. The test runs when any
// interface sent, since discovery tolerates per-interface failures too. It
// skips, naming why, only when none could send for lack of a multicast route
// (or there is no eligible interface), and fails when none could send and an
// error was anything else.
func decideMulticast(probes []interfaceProbe) (skip string, failed error, notes []string) {
	if len(probes) == 0 {
		return "no up, multicast-capable IPv4 interface", nil, nil
	}
	sent := false
	var unexpected error
	var reasons []string
	for _, p := range probes {
		if p.err == nil {
			sent = true
			continue
		}
		reasons = append(reasons, fmt.Sprintf("%s: %v", p.name, p.err))
		if !noMulticastRoute(p.err) {
			unexpected = errors.Join(unexpected, fmt.Errorf("%s: %w", p.name, p.err))
			notes = append(notes, fmt.Sprintf("unexpected send error on %s: %v", p.name, p.err))
		}
	}
	switch {
	case sent:
		return "", nil, notes
	case unexpected != nil:
		return "", unexpected, notes
	default:
		return strings.Join(reasons, "; "), nil, notes
	}
}

// noMulticastRoute reports whether a send failed because the host has no
// route for multicast out of that interface, the environment limitation the
// skip exists for.
func noMulticastRoute(err error) bool {
	return errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EADDRNOTAVAIL)
}

func mdnsProbeQuery() []byte {
	msg := make([]byte, 12)
	binary.BigEndian.PutUint16(msg[4:], 1) // one question
	for _, label := range []string{"_nvpair-ci-probe", "_tcp", "local"} {
		msg = append(msg, byte(len(label)))
		msg = append(msg, label...)
	}
	msg = append(msg, 0, 0, 12, 0, 1) // root, type PTR, class IN
	return msg
}

func TestDecideMulticast(t *testing.T) {
	noRoute := &net.OpError{Op: "write", Net: "udp4", Err: syscall.EHOSTUNREACH}
	odd := &net.OpError{Op: "write", Net: "udp4", Err: syscall.EPERM}
	for _, tc := range []struct {
		name      string
		probes    []interfaceProbe
		wantSkip  bool
		wantFail  bool
		wantNotes int
	}{
		{"no eligible interface", nil, true, false, 0},
		{"one interface can send", []interfaceProbe{{"en0", nil}}, false, false, 0},
		{"any interface sending wins", []interfaceProbe{{"en0", noRoute}, {"en1", nil}}, false, false, 0},
		{"no route anywhere", []interfaceProbe{{"en0", noRoute}, {"utun3", noRoute}}, true, false, 0},
		{"an unexpected error with no sender fails", []interfaceProbe{{"en0", noRoute}, {"en1", odd}}, false, true, 1},
		{"success then unexpected error is noted", []interfaceProbe{{"en0", nil}, {"en1", odd}}, false, false, 1},
		{"unexpected error then success is noted", []interfaceProbe{{"en1", odd}, {"en0", nil}}, false, false, 1},
	} {
		skip, failed, notes := decideMulticast(tc.probes)
		if (skip != "") != tc.wantSkip || (failed != nil) != tc.wantFail || len(notes) != tc.wantNotes {
			t.Errorf("%s: skip=%q failed=%v notes=%q", tc.name, skip, failed, notes)
		}
	}
}
