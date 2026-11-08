// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/noderec"
)

func manualTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func bindTestManager(t *testing.T, m *Manager) {
	t.Helper()
	m.cancel()
	m.ctx, m.cancel = context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(func() {
		m.cancel()
		m.mu.Lock()
		for _, node := range m.nodes {
			if node.cancelTelemetry != nil {
				node.cancelTelemetry()
			}
		}
		m.mu.Unlock()
		m.closeIdleTelemetry()
	})
}

// inventoryPeer serves the fixture's payloads only after checking the caller's
// pin. Node-info advertises this same authenticated endpoint for both services.
// Payloads are supplied through rt so tests can change availability between reads.
type inventoryPeer struct {
	entry      ManualEntry
	server     *httptest.Server
	principal  string
	clusterDir string
	enginePort int
	polls      chan string
	newConns   atomic.Int32
}

func newInventoryPeer(t *testing.T, m *Manager, rt *fakeRoundTripper, alias, principal string) *inventoryPeer {
	t.Helper()
	selfDir := newClusterSelfDir(t)
	peerDir := t.TempDir()
	clustertrusttest.WriteKeypair(t, peerDir, principal)
	clustertrusttest.WriteAdmission(t, peerDir, "cluster-1", 1)
	writePinFromCert(t, selfDir, principal, filepath.Join(peerDir, "node.crt"))
	writePinFromCert(t, peerDir, "uuid-self", filepath.Join(selfDir, "node.crt"))
	m.mesh = clustertrust.Open(selfDir)
	peerMesh := clustertrust.Open(peerDir)
	p := &inventoryPeer{principal: principal, clusterDir: peerDir, polls: make(chan string, 64)}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := peerMesh.VerifyClientPin(r); !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		port := "14322"
		if r.URL.Path == "/v1/node-info" {
			port = "14318"
			select {
			case p.polls <- r.Header.Get("X-NVPAIR-Poll"):
			default:
			}
		}
		req := r.Clone(r.Context())
		req.URL.Host = net.JoinHostPort(alias, port)
		resp, err := rt.RoundTrip(req)
		if err != nil {
			http.Error(w, "fixture unavailable", http.StatusServiceUnavailable)
			return
		}
		defer resp.Body.Close()
		if r.URL.Path == "/v1/node-info" && resp.StatusCode == http.StatusOK {
			var info NodeInfoResponse
			if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
				http.Error(w, "invalid fixture", http.StatusBadGateway)
				return
			}
			info.HostUUID = principal
			info.ClusterUUID = &p.principal
			emPort := p.entry.TLSPort
			if p.enginePort != 0 {
				emPort = p.enginePort
			}
			info.PeerMetadata = &noderec.PeerMetadataParams{HostUUID: principal, Services: map[noderec.ServiceKey]int{
				noderec.ServiceNodeInfo:      p.entry.TLSPort,
				noderec.ServiceEngineManager: emPort,
				noderec.ServiceOllama:        11434,
				noderec.ServiceLMStudio:      1234,
				noderec.ServiceOMLX:          1236,
			}}
			_ = json.NewEncoder(w).Encode(info)
			return
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	srv.TLS = peerMesh.ServerTLSConfig()
	srv.Config.ReadTimeout = 2 * time.Second
	srv.Config.WriteTimeout = 2 * time.Second
	_, portString, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portString)
	if err != nil {
		t.Fatal(err)
	}
	p.entry = ManualEntry{Address: "127.0.0.1", TLSPort: port, MTLS: true}
	p.server = srv
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			p.newConns.Add(1)
		}
	}
	srv.StartTLS()
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	return p
}

func (p *inventoryPeer) track(m *Manager, name string) ManualEntry {
	entry := p.entry
	entry.Name = name
	m.nodes[nodeID(entry)] = &trackedNode{entry: entry, status: ManualNodeStatus{
		ID: nodeID(entry), Address: entry.Address, ClusterUUID: p.principal,
		HostUUID: p.principal, IdentityVerification: "verified", PeerRecordID: m.mesh.PeerRecordID(p.principal),
	}}
	return entry
}

func receivePoll(t *testing.T, ctx context.Context, polls <-chan string) string {
	t.Helper()
	select {
	case poll := <-polls:
		return poll
	case <-ctx.Done():
		t.Fatal("timed out waiting for poll intent")
		return ""
	}
}
