// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nvpair-shared/noderec"
)

func localReadinessFixture(t *testing.T, handler http.Handler) (*daemon, *noderec.DirectoryNode) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	d := newSelfTestDaemon("self", "127.0.0.1")
	return d, &noderec.DirectoryNode{HostUUID: "self", Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceNodeInfo: {Port: port}}}
}

func TestLocalNodeInfoStartupWaitsBeforeWarning(t *testing.T) {
	logs := captureLogs(t)
	var requests atomic.Int32
	d, node := localReadinessFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(NodeInfoResponse{HostUUID: "self"})
	}))
	d.enrichInfoAt(node, "127.0.0.1")
	if requests.Load() < 2 {
		t.Fatal("startup did not retry local node-info readiness")
	}
	if _, ok := d.lastInfo["self"]; !ok {
		t.Fatal("local inventory not read after startup")
	}
	if strings.Contains(logs.String(), "not answering") {
		t.Fatalf("normal startup warned: %s", logs.String())
	}
	// Once it answered, a later outage must warn immediately, not get a new grace.
	d.http.CloseIdleConnections()
	d.http = &http.Client{Transport: readinessUnavailable{}}
	start := time.Now()
	d.enrichInfoAt(node, "127.0.0.1")
	if !strings.Contains(logs.String(), "not answering") || time.Since(start) > time.Second {
		t.Fatalf("later outage was hidden/delayed: %s", logs.String())
	}
}

type readinessUnavailable struct{}

func (readinessUnavailable) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, http.ErrServerClosed
}

func TestLocalNodeInfoRealStartupOutageEventuallyWarns(t *testing.T) {
	logs := captureLogs(t)
	d, node := localReadinessFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 503) }))
	start := time.Now()
	d.enrichInfoAt(node, "127.0.0.1")
	elapsed := time.Since(start)
	if elapsed < 10*time.Second || elapsed > 20*time.Second {
		t.Fatalf("startup readiness wait=%v, want bounded grace", elapsed)
	}
	if got := strings.Count(logs.String(), "not answering"); got != 1 {
		t.Fatalf("outage warnings=%d: %s", got, logs.String())
	}
}
