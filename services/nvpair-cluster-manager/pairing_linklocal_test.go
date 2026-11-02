// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"eapnoob"
)

// The joiner's Completion Exchange POST targets the inviter address carried in
// PairingInfo.Addr. When the inviter was reached over IPv6 link-local, that
// address is only dialable with its scope zone (#69).
func TestScopedHostPreservesZone(t *testing.T) {
	ua := &net.UDPAddr{IP: net.ParseIP("fe80::c16:bee4:c6a1:3fd"), Zone: "en0"}
	if got := scopedHost(ua); got != "fe80::c16:bee4:c6a1:3fd%en0" {
		t.Fatalf("scopedHost = %q, want the scope zone preserved", got)
	}
}

func TestScopedHostWithoutZoneUnchanged(t *testing.T) {
	for _, ip := range []string{"192.168.86.10", "fe80::1"} {
		ua := &net.UDPAddr{IP: net.ParseIP(ip)}
		if got := scopedHost(ua); got != ip {
			t.Errorf("scopedHost(%q) = %q, want unchanged", ip, got)
		}
	}
}

// outboundIP against loopback must keep returning the bare IP (no zone).
func TestOutboundIPLoopbackHasNoZone(t *testing.T) {
	if got := outboundIP("127.0.0.1:9"); got != "127.0.0.1" {
		t.Fatalf("outboundIP = %q, want 127.0.0.1", got)
	}
}

// A raw % in the URL host is rejected as an invalid URL escape, so the zone
// must be percent-encoded (RFC 6874) for the joiner's return POST to parse.
func TestPeerURLZoneEncoding(t *testing.T) {
	got := peerURL("http", "[fe80::c16:bee4:c6a1:3fd%en0]:14321", "/v1/cluster/pairing")
	want := "http://[fe80::c16:bee4:c6a1:3fd%25en0]:14321/v1/cluster/pairing"
	if got != want {
		t.Fatalf("peerURL = %q, want %q", got, want)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("peerURL output does not parse: %v", err)
	}
	if u.Hostname() != "fe80::c16:bee4:c6a1:3fd%en0" || u.Port() != "14321" {
		t.Fatalf("reparsed host = %q port = %q, zone lost", u.Hostname(), u.Port())
	}
}

func TestPeerURLPlainHostportUnchanged(t *testing.T) {
	got := peerURL("https", "192.168.86.10:14321", "/v1/cluster/members/remove")
	want := "https://192.168.86.10:14321/v1/cluster/members/remove"
	if got != want {
		t.Fatalf("peerURL = %q, want %q", got, want)
	}
}

// A zone names an interface of the machine that wrote it. The inviter's
// advertised %en0 means nothing to a joiner whose interface is eth0; the
// joiner dials with the zone of the connection the inviter reached it on.
func TestLocalScopeUsesTheReceivingInterface(t *testing.T) {
	for _, tc := range []struct{ advertised, via, want string }{
		{"[fe80::1%en0]:14321", "fe80::1%eth0", "[fe80::1%eth0]:14321"},
		{"[fe80::1]:14321", "fe80::1%eth0", "[fe80::1%eth0]:14321"},
		{"[fe80::1%en0]:14321", "192.168.1.5", "[fe80::1]:14321"},
		{"192.168.1.5:14321", "fe80::1%eth0", "192.168.1.5:14321"},
		{"[2001:db8::1]:14321", "fe80::1%eth0", "[2001:db8::1]:14321"},
	} {
		if got := localScope(tc.advertised, tc.via); got != tc.want {
			t.Errorf("localScope(%q, %q) = %q, want %q", tc.advertised, tc.via, got, tc.want)
		}
	}
}

type recordingTransport struct{ urls []string }

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.urls = append(r.urls, req.URL.String())
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
}

// Decline, fail and expire signals go to the same scoped addresses as the
// pairing exchange, so they need the same URL encoding.
func TestPairingSignalReachesAScopedTarget(t *testing.T) {
	rt := &recordingTransport{}
	client := &http.Client{Transport: rt}
	for _, scheme := range []string{"http", "https"} {
		if err := postPairingSignalScheme(client, scheme, "[fe80::1%en0]:14321", "inv-1", "decline", ""); err != nil {
			t.Fatalf("%s: %v", scheme, err)
		}
	}
	want := []string{"http://[fe80::1%25en0]:14321/v1/cluster/pairing", "https://[fe80::1%25en0]:14321/v1/cluster/pairing"}
	if len(rt.urls) != 2 || rt.urls[0] != want[0] || rt.urls[1] != want[1] {
		t.Fatalf("urls = %v, want %v", rt.urls, want)
	}
}

// The joiner dials, and records as the member's address, the inviter's
// advertised link-local address with the zone of its own receiving interface,
// not the inviter's.
func TestJoinerRecordsTheInviterWithItsOwnZone(t *testing.T) {
	inviter := testManagerAt(t, t.TempDir(), 15221)
	joiner := testManagerAt(t, t.TempDir(), 15222)
	inviterEpoch, err := inviter.reserveAdmissionEpoch()
	if err != nil {
		t.Fatal(err)
	}
	joinerEpoch, err := joiner.reserveAdmissionEpoch()
	if err != nil {
		t.Fatal(err)
	}
	serverInfo, err := inviter.localPairingInfo("[fe80::1%en0]:15221", inviterEpoch).toMap()
	if err != nil {
		t.Fatal(err)
	}
	peerInfo, err := joiner.localPairingInfo("[fe80::2%eth0]:15222", joinerEpoch).toMap()
	if err != nil {
		t.Fatal(err)
	}
	server := newPairingServer(serverInfo)
	peer := newPairingPeer(peerInfo)
	msg, err := server.Start()
	if err != nil {
		t.Fatal(err)
	}
	for peerTurn := true; len(msg) > 0; peerTurn = !peerTurn {
		var out eapnoob.Outcome
		if peerTurn {
			out, err = peer.Receive(msg)
		} else {
			out, err = server.Receive(msg)
		}
		if err != nil || out.Err != nil {
			t.Fatalf("initial exchange: %v %v", err, out.Err)
		}
		msg = out.Send
	}
	sess := &pairingSession{inviteID: "inv-ll", role: roleJoiner, peer: peer}
	if joiner.prepareJoinerInitialComplete("inv-ll", sess, "fe80::1%eth0") == nil {
		t.Fatal("no invite prepared")
	}
	want := "[fe80::1%eth0]:15221"
	if sess.addr != want || sess.peerPairing.Addr != want {
		t.Fatalf("dial %q, recorded %q, want %q for both", sess.addr, sess.peerPairing.Addr, want)
	}
}
