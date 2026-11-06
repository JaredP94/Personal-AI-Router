// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRoamingNewCandidateNeedsDurableDialBudget(t *testing.T) {
	m, b := newTestManager(t), newTestManager(t)
	roamingMember(t, m, b)
	var hits atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(200) }))
	srv.TLS = b.buildServerTLSConfig()
	srv.StartTLS()
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "https://")
	m.memMu.Lock()
	m.members[b.identity.NodeUUID].addresses.AddressCandidates = []addressCandidate{{Address: addr, LearnedAt: time.Now().UnixMilli()}}
	m.memMu.Unlock()
	if err := os.Mkdir(m.membersPath(), 0700); err != nil {
		t.Fatal(err)
	} // atomic rename must fail
	m.reconcileWith([]string{addr}, b.identity.NodeUUID)
	if hits.Load() != 0 {
		t.Fatal("dialed unproven hint without persisting its retry budget")
	}
}
