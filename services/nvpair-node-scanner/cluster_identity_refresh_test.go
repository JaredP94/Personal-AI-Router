// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/noderec"
	"strconv"
	"testing"
)

// Public membership used to override cluster annotations and confer trust. It
// is now only a preview; lack of authenticated metadata cannot establish a peer.
func TestPublicIdentityRefreshCannotChangeMembership(t *testing.T) {
	for _, body := range []string{`{"hostUuid":"uuid-peer","clusterUuid":"uuid-peer"}`, `{"hostUuid":"uuid-peer","clusterUuid":""}`, `{"clusterUuid":"uuid-peer"}`, `{"hostUuid":"other-peer","clusterUuid":"uuid-peer"}`} {
		t.Run(body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer srv.Close()
			host, p, _ := net.SplitHostPort(srv.Listener.Addr().String())
			port, _ := strconv.Atoi(p)
			d := newSelfTestDaemon("uuid-self", "192.0.2.1")
			old := noderec.DirectoryNode{IdentityVerification: "unverified", PeerRecordID: noderec.PreviewRecordID("candidate", "uuid-peer"), HostUUID: "uuid-peer", ClusterUUID: "uuid-peer", IP: host, Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceNodeInfo: {Port: port}}}
			d.dir.upsert(old)
			if d.refreshNodeClusterIdentity(old.RecordKey(), host, port) {
				t.Fatal("public response changed membership")
			}
			got, _ := d.dir.get(old.RecordKey())
			if got.Trusted || got.ClusterUUID != old.ClusterUUID || got.PeerRecordID != old.PeerRecordID {
				t.Fatalf("public membership applied: %+v", got)
			}
		})
	}
}

func TestAuthenticatedRefreshPromotesPendingPeerWithoutMDNSChange(t *testing.T) {
	d, srv, mismatch := secureInventoryFixture(t, "uuid-peer")
	host, p, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(p)
	mismatch.Store(true)
	d.onBrowse(DiscoveryEvent{Type: "discovered", Node: RawNode{ID: "pending", Addresses: []string{host}, TXT: []string{"v=1", "uuid=uuid-peer", "cluster-uuid=uuid-peer", "ip=" + host, "ni=" + p}}})
	mismatch.Store(false)
	if !d.refreshNodeClusterIdentityCandidates(candidateRecordKey(RawNode{ID: "pending", Addresses: []string{host}, TXT: []string{"v=1", "uuid=uuid-peer", "cluster-uuid=uuid-peer", "ip=" + host, "ni=" + p}}), []string{"127.0.0.2", host}, port) {
		t.Fatal("pending peer not authenticated via answering address")
	}
	got, _ := d.dir.get("uuid-peer")
	if !got.ServicePeer() || got.IP != host || got.Services[noderec.ServiceOllama].Port != 11434 {
		t.Fatalf("promoted peer = %+v", got)
	}
	clustertrusttest.RemovePeerPin(t, d.clusterDir, "uuid-peer")
	d.reloadTrust()
	got, _ = d.dir.get("uuid-peer")
	if got.Trusted || got.ServicePeer() || len(got.GPUs) > 0 {
		t.Fatalf("pin removal retained inventory: %+v", got)
	}
}

func TestAuthenticatedResponseCannotNameAnotherHost(t *testing.T) {
	d, srv, mismatch := secureInventoryFixture(t, "uuid-peer")
	host, p, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(p)
	mismatch.Store(true)
	node := noderec.DirectoryNode{HostUUID: "uuid-peer", ClusterUUID: "uuid-peer"}
	if _, _, ok := d.authenticatedInfo(t.Context(), node, []string{host}, port); ok {
		t.Fatal("authenticated certificate accepted a different response identity")
	}
}
