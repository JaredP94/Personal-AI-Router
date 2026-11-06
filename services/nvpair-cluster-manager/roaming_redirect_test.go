// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRoamingPinnedPeerCannotRedirectCandidateDials(t *testing.T) {
	m, b := newTestManager(t), newTestManager(t)
	roamingMember(t, m, b)
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer target.Close()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	srv.TLS = b.buildServerTLSConfig()
	srv.StartTLS()
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "https://")
	host, port := splitAddr(addr, m.port)
	m.upsertMember(&ClusterNode{NodeUUID: b.identity.NodeUUID, ID: "peer", IPAddress: host, Port: port, ClusterID: "cluster-1", AdmissionEpoch: 1, State: stateMember})
	m.reconcileWith([]string{addr}, b.identity.NodeUUID)
	if hits.Load() != 0 {
		t.Fatal("pinned peer redirected a dial outside the candidate budget")
	}
}
