// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRoamingCandidateValidationAndIPv6Zones(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:1", "[::]:1", "255.255.255.255:1", "224.0.0.1:1", "[ff02::1%en0]:1", "[fe80::1]:1", "[2001:db8::1%en0]:1", "[fe80::1%bad/name]:1", "127.0.0.1:1", "169.254.1.2:1", "example.test:1", "10.0.0.1:65536", "10.0.0.1:0"} {
		if got, ok := candidateEndpoint(addr, false); ok {
			t.Errorf("accepted %q as %q", addr, got)
		}
	}
	for _, addr := range []string{"100.64.0.1:14321", "192.168.1.1:14321", "[2001:db8::1]:14321", "[fe80::1%en0]:14321"} {
		if got, ok := candidateEndpoint(addr, false); !ok || got != addr {
			t.Errorf("rejected/changed %q: %q %v", addr, got, ok)
		}
	}
	m, b := newTestManager(t), newTestManager(t)
	roamingMember(t, m, b)
	roamingRequest(t, m, b, "[fe80::2%local0]:50000", []string{"[fe80::3%remote0]:14999"})
	n, _ := m.memberByNodeID(b.identity.NodeUUID)
	got := m.resolvePeerAddrs(*n)
	if !slices.Contains(got, "[fe80::3%local0]:14999") || slices.Contains(got, "[fe80::3%remote0]:14999") {
		t.Fatalf("remote scope leaked into dial: %v", got)
	}
	if !strings.Contains(peerURL("https", "[fe80::3%local0]:14999", rosterPath), "%25local0") {
		t.Fatal("IPv6 zone not URL escaped")
	}
}

func TestRoamingNewCandidateDialBudgetSurvivesClaimChurnAndReload(t *testing.T) {
	m, b := newTestManager(t), newTestManager(t)
	roamingMember(t, m, b)
	hints := []string{"100.64.0.1:14999", "100.64.0.2:14999", "100.64.0.3:14999"}
	roamingRequest(t, m, b, "10.0.0.1:50000", hints)
	now := time.Now()
	n, _ := m.memberByNodeID(b.identity.NodeUUID)
	first, reserved := m.limitCandidateDials(n.NodeUUID, m.resolvePeerAddrs(*n), now)
	if !reserved || len(first) != 3 || !slices.Contains(first, hints[0]) {
		t.Fatalf("first probe: %v %v", first, reserved)
	}
	m.persistMembers()
	m.resetMembership()
	if err := m.loadMembers(); err != nil {
		t.Fatal(err)
	}
	roamingRequest(t, m, b, "10.0.0.1:50000", []string{"100.64.0.4:14999"})
	n, _ = m.memberByNodeID(b.identity.NodeUUID)
	second, reserved := m.limitCandidateDials(n.NodeUUID, m.resolvePeerAddrs(*n), now.Add(time.Second))
	if reserved || len(second) != 2 {
		t.Fatalf("new claims/restart reset rate limit: %v %v", second, reserved)
	}
	third, reserved := m.limitCandidateDials(n.NodeUUID, m.resolvePeerAddrs(*n), now.Add(31*time.Second))
	if !reserved || !slices.Contains(third, hints[1]) {
		t.Fatalf("untried hint starved: %v %v", third, reserved)
	}
}

type roamingBlockedBody struct {
	entered, release chan struct{}
	reader           io.Reader
}

func (b *roamingBlockedBody) Read(p []byte) (int, error) {
	select {
	case <-b.entered:
	default:
		close(b.entered)
	}
	<-b.release
	return b.reader.Read(p)
}
func (b *roamingBlockedBody) Close() error { return nil }

func TestRoamingInFlightRequestCannotRestoreRemovedMember(t *testing.T) {
	m, b := newTestManager(t), newTestManager(t)
	roamingMember(t, m, b)
	blocked := &roamingBlockedBody{entered: make(chan struct{}), release: make(chan struct{}), reader: strings.NewReader(`{"clusterId":"cluster-1","selfAddresses":["100.64.0.8:14999"]}`)}
	r := httptest.NewRequest(http.MethodPost, rosterPath, nil)
	r.Body = blocked
	r.RemoteAddr = "10.0.0.8:50000"
	cert, err := x509.ParseCertificate(b.identity.Cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); m.handleRoster(w, r) }()
	<-blocked.entered
	m.rosterMu.Lock()
	if err := m.trust.Remove(b.identity.NodeUUID); err != nil {
		t.Fatal(err)
	}
	m.removeMemberByUUID(b.identity.NodeUUID)
	m.persistMembers()
	m.rosterMu.Unlock()
	close(blocked.release)
	<-done
	if w.Code != http.StatusForbidden {
		t.Fatalf("stale request accepted: %d", w.Code)
	}
	m.resetMembership()
	if err := m.loadMembers(); err != nil {
		t.Fatal(err)
	}
	if _, exists := m.memberByNodeID(b.identity.NodeUUID); exists {
		t.Fatal("in-flight learning resurrected removed member")
	}
}

func TestRoamingLearnsSelfReportFromPinnedResponseOnly(t *testing.T) {
	m, b := newTestManager(t), newTestManager(t)
	roamingMember(t, m, b)
	const hint = "100.64.0.8:14999"
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := b.verifyClientPin(r); !ok {
			http.Error(w, "forbidden", 403)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"clusterId": "cluster-1", "selfAddresses": []string{hint}})
	}))
	pinTrusted(t, b, m.identity.NodeUUID, string(m.identity.CertPEM), m.identity.CertFingerprint)
	srv.TLS = b.buildServerTLSConfig()
	srv.StartTLS()
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "https://")
	host, port := splitAddr(addr, m.port)
	m.upsertMember(&ClusterNode{NodeUUID: b.identity.NodeUUID, ID: "peer", IPAddress: host, Port: port, ClusterID: "cluster-1", AdmissionEpoch: 1, State: stateMember})
	outcome, _ := m.reconcileWithin(context.Background(), []string{addr}, b.identity.NodeUUID)
	if outcome != reconcileAccepted {
		t.Fatalf("outcome=%v", outcome)
	}
	n, _ := m.memberByNodeID(b.identity.NodeUUID)
	if !slices.Contains(m.resolvePeerAddrs(*n), hint) {
		t.Fatal("pinned response self-report not learned")
	}
}
