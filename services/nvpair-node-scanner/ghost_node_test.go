// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// These probes govern only public candidate-preview lifetime. A public identity
// response may retire a stale preview, never a verified peer. The former
// cross-record public identity eviction path has been removed; authenticated
// proof expiry and independent record keys now own verified-peer lifetime.

// nodeInfoServer starts a /v1/node-info endpoint reporting hostUUID and returns
// its host and port. An empty hostUUID omits the field, mimicking a peer that
// predates node-info reporting identity.
func nodeInfoServer(t *testing.T, hostUUID string) (host string, port int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/node-info" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(NodeInfoResponse{HostUUID: hostUUID})
	}))
	t.Cleanup(srv.Close)
	return splitHostPort(t, strings.TrimPrefix(srv.URL, "http://"))
}

// openPort leaves a bare TCP listener open and returns its port. It stands in
// for a service that accepts connections but proves nothing about identity.
func openPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	_, port := splitHostPort(t, ln.Addr().String())
	return port
}

// closedPort returns a port nothing is listening on, by binding and releasing
// it. Using a bound-then-released port rather than a low well-known one keeps
// the dial a prompt refusal on every platform.
func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, port := splitHostPort(t, ln.Addr().String())
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return port
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port %q: %v", portStr, err)
	}
	return host, port
}

// probeDaemon is the minimum daemon the liveness probe needs.
func probeDaemon() *daemon {
	return &daemon{http: &http.Client{Timeout: nodeInfoFetchTimeout}}
}

func rawNode(txt ...string) RawNode {
	return RawNode{ID: "wiped-host", Addresses: []string{"127.0.0.1"}, TXT: txt}
}

// TestReachableRejectsSupersededIdentity is the core of the fix. The ghost's
// cached address and ports are live — they belong to the machine's NEW
// incarnation — so a TCP-only probe reports the stale record alive forever. The
// probe must ask WHO answers, not just WHETHER anything answers.
func TestReachableRejectsSupersededIdentity(t *testing.T) {
	host, port := nodeInfoServer(t, "new-uuid-after-wipe")
	d := probeDaemon()

	n := rawNode("v=1", "uuid=old-uuid-before-wipe", "ip="+host, fmt.Sprintf("ni=%d", port))
	if d.reachable(n) {
		t.Fatal("a record whose address now answers with a DIFFERENT hostUuid must not be kept alive")
	}
}

// TestReachableAcceptsMatchingIdentity guards the other direction: a peer that
// merely dropped off multicast must survive the probe, or a transient mDNS gap
// would evict healthy nodes.
func TestReachableAcceptsMatchingIdentity(t *testing.T) {
	host, port := nodeInfoServer(t, "steady-uuid")
	d := probeDaemon()

	n := rawNode("v=1", "uuid=steady-uuid", "ip="+host, fmt.Sprintf("ni=%d", port))
	if !d.reachable(n) {
		t.Fatal("a record whose address confirms the same hostUuid must be kept alive")
	}
}

// TestReachableFallsBackWithoutIdentity covers every way identity can be
// unavailable. None of them may change today's behaviour: an open ni/em port
// still means alive, so no node that used to survive a probe starts evicting.
// Inference ports (ol/lm) are never used as the TCP fallback.
func TestReachableFallsBackWithoutIdentity(t *testing.T) {
	tcpPort := openPort(t)
	niHost, niPort := nodeInfoServer(t, "") // answers, but reports no identity

	cases := []struct {
		name string
		txt  []string
	}{
		{
			name: "no ni advertised",
			txt:  []string{"v=1", "uuid=some-uuid", "ip=127.0.0.1", fmt.Sprintf("em=%d", tcpPort)},
		},
		{
			name: "node-info not answering",
			txt: []string{"v=1", "uuid=some-uuid", "ip=127.0.0.1",
				fmt.Sprintf("ni=%d", closedPort(t)), fmt.Sprintf("em=%d", tcpPort)},
		},
		{
			name: "node-info reports no hostUuid",
			txt: []string{"v=1", "uuid=some-uuid", "ip=" + niHost,
				fmt.Sprintf("ni=%d", niPort), fmt.Sprintf("em=%d", tcpPort)},
		},
		{
			name: "record carries no uuid to compare",
			txt:  []string{"v=1", "ip=127.0.0.1", fmt.Sprintf("ni=%d", niPort), fmt.Sprintf("em=%d", tcpPort)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !probeDaemon().reachable(rawNode(tc.txt...)) {
				t.Fatal("with identity unavailable the probe must fall back to the TCP sweep")
			}
		})
	}
}

// TestReachableEvictsWhenNothingAnswers keeps the base case honest: a node that
// is genuinely gone still evicts.
func TestReachableEvictsWhenNothingAnswers(t *testing.T) {
	d := probeDaemon()
	dead := rawNode("v=1", "uuid=gone", "ip=127.0.0.1",
		fmt.Sprintf("ni=%d", closedPort(t)), fmt.Sprintf("ol=%d", closedPort(t)))
	if d.reachable(dead) {
		t.Fatal("a node answering on nothing must not be kept alive")
	}
	if d.reachable(rawNode("v=1", "uuid=identity-only", "ip=127.0.0.1")) {
		t.Fatal("a node advertising no services has nothing to probe and must evict")
	}
}

// countingTransport records how many HTTP requests the probe actually issues.
type countingTransport struct {
	n int
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n++
	return http.DefaultTransport.RoundTrip(r)
}

// TestReachableSkipsIdentityWhenNothingAnswers pins the probe's ORDER, not just
// its verdicts. The identity read must run only when the TCP sweep already found
// the address answering — i.e. only when the node would otherwise have been
// kept, which is the sole case where the two need telling apart.
//
// Asking the other way round is what this guards against: it would spend a
// node-info timeout on every departed node, serially, inside the browser's scan
// cycle. The verdicts are identical either way, so nothing but a request count
// can catch a regression here.
func TestReachableSkipsIdentityWhenNothingAnswers(t *testing.T) {
	counter := &countingTransport{}
	d := &daemon{http: &http.Client{Transport: counter, Timeout: nodeInfoFetchTimeout}}

	dead := rawNode("v=1", "uuid=gone", "ip=127.0.0.1",
		fmt.Sprintf("ni=%d", closedPort(t)), fmt.Sprintf("ol=%d", closedPort(t)))
	if d.reachable(dead) {
		t.Fatal("a node answering on nothing must not be kept alive")
	}
	if counter.n != 0 {
		t.Fatalf("a departed node cost %d node-info request(s); the TCP sweep must settle it alone", counter.n)
	}

	// When the address DOES answer, the identity read is exactly what decides it.
	host, port := nodeInfoServer(t, "someone-else")
	live := rawNode("v=1", "uuid=old-uuid", "ip="+host, fmt.Sprintf("ni=%d", port))
	if d.reachable(live) {
		t.Fatal("an answering address reporting a different hostUuid must evict")
	}
	if counter.n != 1 {
		t.Fatalf("node-info requests = %d, want exactly 1 once the address answered", counter.n)
	}
}
