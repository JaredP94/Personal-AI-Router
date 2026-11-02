// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import "testing"

// expectStateMismatch asserts that a message was refused as out of order: a
// terminal ErrStateMismatch, no success, and no transport error.
func expectStateMismatch(t *testing.T, out Outcome, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("Receive returned err %v; an out-of-order message must surface in out.Err", err)
	}
	if out.Err == nil || out.Err.Code != ErrStateMismatch {
		t.Fatalf("out.Err = %v, want code %d (state mismatch)", out.Err, ErrStateMismatch)
	}
	if !out.Done || out.Success {
		t.Fatalf("out Done=%v Success=%v, want a terminal failure", out.Done, out.Success)
	}
}

// A completion message can arrive while the server is waiting for the PIN,
// before its authentication keys exist. Before PIN verification the transcript
// is public, so an empty-key MAC is computable without the PIN and must not
// grant trust.
func TestServerRejectsCompletionBeforeKeysAreDerived(t *testing.T) {
	for _, suite := range []int{1, 2} {
		srv := NewServer(ServerConfig{Cryptosuites: []int{suite}, Dirs: 2}, nil)
		peer := NewPeer(PeerConfig{PreferDir: 2}, nil)
		driveConversation(t, srv, peer)
		if srv.State() != StateWaiting || len(srv.keyM.Kmp) != 0 {
			t.Fatalf("suite %d: server state=%s Kmp=%d, want Waiting with no keys", suite, srv.State(), len(srv.keyM.Kmp))
		}

		macp := srv.suite.computeMAC(nil, 1, srv.in)
		msg, _, err := encode(&wireMessage{
			Type:   intp(typeCompletion),
			PeerId: rawString(srv.peerId),
			MACp:   rawString(b64Encode(macp)),
		})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}

		out, err := srv.Receive(msg)
		expectStateMismatch(t, out, err)
		if srv.State() == StateRegistered {
			t.Fatalf("suite %d: server registered on an empty-key MAC", suite)
		}
	}
}

// A sender can answer discovery with a key-exchange response before
// negotiation selects an algorithm and creates the server's private key.
func TestServerRejectsKeyExchangeBeforeNegotiation(t *testing.T) {
	srv := NewServer(ServerConfig{}, nil)
	if _, err := srv.Start(); err != nil {
		t.Fatalf("server Start: %v", err)
	}
	out, err := srv.Receive([]byte(`{"Type":3,"PKp":{},"Np":""}`))
	expectStateMismatch(t, out, err)
	if srv.State() != StateUnregistered {
		t.Fatalf("server state %s, want Unregistered", srv.State())
	}
}

// A sender can skip negotiation after discovery. The peer must reject its
// key-exchange message before touching the algorithm negotiation selects.
func TestPeerRejectsKeyExchangeBeforeNegotiation(t *testing.T) {
	peer := NewPeer(PeerConfig{}, nil)
	discovery, err := peer.Receive([]byte(`{"Type":1}`))
	if err != nil || discovery.Err != nil {
		t.Fatalf("discovery: err=%v out.Err=%v", err, discovery.Err)
	}
	out, err := peer.Receive([]byte(`{"Type":3,"PKs":{},"Ns":""}`))
	expectStateMismatch(t, out, err)
	if peer.State() != StateUnregistered {
		t.Fatalf("peer state %s, want Unregistered", peer.State())
	}
}

// EAP-Success only means something once the peer has verified the server's
// Completion MAC. Earlier, it must not register the association.
func TestPeerRejectsSuccessBeforeVerifiedCompletion(t *testing.T) {
	srv := NewServer(ServerConfig{Dirs: 2}, nil)
	peer := NewPeer(PeerConfig{PreferDir: 2}, nil)
	driveConversation(t, srv, peer)

	out, err := peer.Receive(resultBytes(eapSuccess))
	expectStateMismatch(t, out, err)
	if peer.State() == StateRegistered {
		t.Fatal("peer registered on an unverified EAP-Success")
	}
	if _, ok, _ := peer.store.Load(peer.peerId); ok {
		t.Fatal("peer stored an association on an unverified EAP-Success")
	}
}

// Any message between the verified Completion and EAP-Success clears the
// verification, a malformed one included.
func TestPeerSuccessNeedsTheCompletionDirectlyBeforeIt(t *testing.T) {
	srv := NewServer(ServerConfig{Dirs: 2}, nil)
	peer := NewPeer(PeerConfig{PreferDir: 2}, nil)
	driveConversation(t, srv, peer)
	peer.completionVerified = true // as after a verified Completion request
	if _, err := peer.Receive([]byte("not json")); err != nil {
		t.Fatalf("malformed input: %v", err)
	}
	out, err := peer.Receive(resultBytes(eapSuccess))
	expectStateMismatch(t, out, err)
}
