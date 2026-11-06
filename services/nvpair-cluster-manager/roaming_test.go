// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func roamingMember(t *testing.T, m, peer *Manager) {
	t.Helper()
	pinTrusted(t, m, peer.identity.NodeUUID, string(peer.identity.CertPEM), peer.identity.CertFingerprint)
	m.upsertMember(&ClusterNode{ID: peer.identity.NodeID, NodeUUID: peer.identity.NodeUUID, Name: "peer", IPAddress: "192.0.2.20", Port: peer.port, ClusterID: "cluster-1", AdmissionEpoch: 1, State: stateMember})
}

func roamingRequest(t *testing.T, m, peer *Manager, source string, hints []string) int {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{"clusterId": "cluster-1", "selfAddresses": hints})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, rosterPath, bytes.NewReader(body))
	cert, err := x509.ParseCertificate(peer.identity.Cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	r.RemoteAddr = source
	w := httptest.NewRecorder()
	m.handleRoster(w, r)
	return w.Code
}

func TestRoamingSelfReportsBelongOnlyToPinnedSender(t *testing.T) {
	m, b, c, unknown := newTestManager(t), newTestManager(t), newTestManager(t), newTestManager(t)
	roamingMember(t, m, b)
	roamingMember(t, m, c)
	hints := []string{"100.64.0.8:14999", "[2001:db8::8]:14999"}
	if code := roamingRequest(t, m, unknown, "10.0.0.8:50000", hints); code != http.StatusForbidden {
		t.Fatalf("unknown cert: %d", code)
	}
	if code := roamingRequest(t, m, c, "10.0.0.9:50000", hints); code != http.StatusOK {
		t.Fatalf("pinned cert: %d", code)
	}
	bn, _ := m.memberByNodeID(b.identity.NodeUUID)
	if got := m.resolvePeerAddrs(*bn); !slices.Equal(got, []string{"192.0.2.20:14999"}) {
		t.Fatalf("another cert taught B addresses: %v", got)
	}
	cn, _ := m.memberByNodeID(c.identity.NodeUUID)
	got := m.resolvePeerAddrs(*cn)
	if !slices.Contains(got, hints[0]) || !slices.Contains(got, hints[1]) {
		t.Fatalf("verified self-report was not learned: %v", got)
	}
}

func TestRoamingCandidatesBoundedOrderedPersistedAndExpired(t *testing.T) {
	m, b := newTestManager(t), newTestManager(t)
	roamingMember(t, m, b)
	var hints []string
	for i := 1; i <= 30; i++ {
		hints = append(hints, "100.64.1."+strconv.Itoa(i)+":14999")
	}
	// Many valid endpoints, followed by the most recently observed connection.
	hints = append(hints, "100.64.0.1:14999", "100.64.0.2:14999", "100.64.0.3:14999", "100.64.0.4:14999", "100.64.0.5:14999", "100.64.0.6:14999", "100.64.0.7:14999", "100.64.0.8:14999", "100.64.0.9:14999")
	roamingRequest(t, m, b, "10.0.0.1:50000", hints)
	roamingRequest(t, m, b, "10.0.0.2:50000", nil)
	n, _ := m.memberByNodeID(b.identity.NodeUUID)
	got := m.resolvePeerAddrs(*n)
	if len(got) != 8 || got[0] != "10.0.0.2:14999" || got[1] != "10.0.0.1:14999" || got[len(got)-1] != "192.0.2.20:14999" {
		t.Fatalf("candidate order/bound/fallback: %v", got)
	}
	if err := m.persistMembersErr(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(m.membersPath())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Fatalf("permissions: %v", st.Mode())
	}
	m.resetMembership()
	if err := m.loadMembers(); err != nil {
		t.Fatal(err)
	}
	n, _ = m.memberByNodeID(b.identity.NodeUUID)
	if after := m.resolvePeerAddrs(*n); !slices.Equal(after, got) {
		t.Fatalf("reload: %v != %v", after, got)
	}
	// Age every learned hint through the actual persistence format, then reload.
	data, _ := os.ReadFile(m.membersPath())
	var stored []struct {
		ClusterNode
		PairingAddress    string `json:"pairingAddress"`
		AddressCandidates []struct {
			Address     string `json:"address"`
			LearnedAt   int64  `json:"learnedAt"`
			LastSuccess int64  `json:"lastSuccess"`
		} `json:"addressCandidates"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || len(stored[0].AddressCandidates) == 0 {
		t.Fatal("learned candidates not persisted")
	}
	for i := range stored[0].AddressCandidates {
		stored[0].AddressCandidates[i].LearnedAt = time.Now().Add(-31 * 24 * time.Hour).UnixMilli()
		stored[0].AddressCandidates[i].LastSuccess = stored[0].AddressCandidates[i].LearnedAt
	}
	data, _ = json.Marshal(stored)
	if err := os.WriteFile(m.membersPath(), data, 0600); err != nil {
		t.Fatal(err)
	}
	m.resetMembership()
	if err := m.loadMembers(); err != nil {
		t.Fatal(err)
	}
	n, _ = m.memberByNodeID(b.identity.NodeUUID)
	if after := m.resolvePeerAddrs(*n); !slices.Equal(after, []string{"192.0.2.20:14999"}) {
		t.Fatalf("expired hints survived: %v", after)
	}
	m.removeMemberByUUID(b.identity.NodeUUID)
	m.persistMembers()
	roamingRequest(t, m, b, "10.0.0.3:50000", hints)
	if _, exists := m.memberByNodeID(b.identity.NodeUUID); exists {
		t.Fatal("removed member resurrected by hint")
	}
}

func TestRoamingIgnoresDiscoveryAndRelayedAddresses(t *testing.T) {
	m, b, c := newTestManager(t), newTestManager(t), newTestManager(t)
	roamingMember(t, m, b)
	roamingMember(t, m, c)
	m.browser = newBrowser()
	m.browser.seed(b.identity.NodeUUID, "10.0.0.99", 14321)
	m.mergeRoster(&Roster{ClusterID: "cluster-1", Members: []RosterEntry{{NodeUUID: b.identity.NodeUUID, Addr: "10.0.0.98:14321"}}}, c.identity.NodeUUID)
	n, _ := m.memberByNodeID(b.identity.NodeUUID)
	if got := m.resolvePeerAddrs(*n); !slices.Equal(got, []string{"192.0.2.20:14999"}) {
		t.Fatalf("unauthenticated/relayed address became learned authority: %v", got)
	}
}

func TestRoamingHostileHintCannotRedirectPinnedIdentity(t *testing.T) {
	m, b, hostile := newTestManager(t), newTestManager(t), newTestManager(t)
	roamingMember(t, m, b)
	hits := 0
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_ = json.NewEncoder(w).Encode(hostile.buildLocalRoster())
	}))
	srv.TLS = hostile.buildServerTLSConfig()
	srv.StartTLS()
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "https://")
	host, port := splitAddr(addr, m.port)
	m.upsertMember(&ClusterNode{NodeUUID: b.identity.NodeUUID, ID: "peer", IPAddress: host, Port: port, ClusterID: "cluster-1", AdmissionEpoch: 1, State: stateMember})
	outcome, _ := m.reconcileWith([]string{addr}, b.identity.NodeUUID)
	if outcome != reconcileUnreachable || hits != 0 {
		t.Fatalf("hostile address redirected identity: outcome=%v hits=%d", outcome, hits)
	}
}

func TestRoamingInboundSourceSurvivesRestart(t *testing.T) {
	m, b := newTestManager(t), newTestManager(t)
	roamingMember(t, m, b)
	roamingRequest(t, m, b, "10.0.0.2:50000", nil)
	m.resetMembership()
	if err := m.loadMembers(); err != nil {
		t.Fatal(err)
	}
	n, _ := m.memberByNodeID(b.identity.NodeUUID)
	got := m.resolvePeerAddrs(*n)
	if !slices.Equal(got, []string{"10.0.0.2:14999", "192.0.2.20:14999"}) {
		t.Fatalf("restart lost source or pairing fallback: %v", got)
	}
}
