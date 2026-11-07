// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"nvpair-shared/clustertrust"
	"nvpair-shared/noderec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"nvpair-shared/clustertrusttest"
)

// A public inventory and an mDNS principal are claims, even when that principal
// has a pin. Neither proves that the machine serving them possesses its key.
func TestClaimedPinnedPrincipalCannotPublishServiceInventory(t *testing.T) {
	const peer = "principal-peer"
	dir := filepath.Join(t.TempDir(), "cluster")
	clustertrusttest.Join(t, dir, "cluster-test", "principal-self", peer)
	d := newMembershipTestDaemon(dir)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string][]string{"models": {"forged-model"}})
			return
		}
		_, portString, _ := net.SplitHostPort(r.Host)
		port, _ := strconv.Atoi(portString)
		principal := peer
		_ = json.NewEncoder(w).Encode(NodeInfoResponse{
			HostUUID: peer, ClusterUUID: &principal, TelemetryValid: true,
			GPUs: []GPUInfo{{Name: "forged-gpu", UtilizationPercent: gpuReading(uint32(99))}},
			PeerMetadata: &noderec.PeerMetadataParams{HostUUID: peer, Name: "forged-peer", Services: map[noderec.ServiceKey]int{
				noderec.ServiceNodeInfo: port, noderec.ServiceEngineManager: port, noderec.ServiceOllama: 11434,
			}},
		})
	}))
	defer srv.Close()
	d.http = srv.Client()
	d.modelsHTTP = srv.Client()
	host, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	d.onBrowse(DiscoveryEvent{Type: "discovered", Node: RawNode{ID: "forged-peer", Addresses: []string{host}, TXT: []string{"v=1", "uuid=" + peer, "cluster-uuid=" + peer, "ip=" + host, "ni=" + port, "em=" + port, "ol=11434"}}})
	if n, ok := d.dir.get(peer); ok && (n.Trusted || len(n.Models) > 0 || len(n.ModelsByEngine) > 0) {
		t.Errorf("unauthenticated claim reached trusted/routing inventory: %+v", n)
	}
	p, _ := strconv.Atoi(port)
	if d.refreshNodeTelemetryCandidates(t.Context(), peer, []string{host}, p) {
		t.Error("plaintext inventory accepted as scheduler telemetry")
	}
}

// secureInventoryFixture uses real mutually pinned cluster leaves and listeners;
// no synthetic TLS state or mock transport can accidentally confer provenance.
func secureInventoryFixture(t *testing.T, principal string) (*daemon, *httptest.Server, *atomic.Bool) {
	t.Helper()
	selfDir, peerDir := t.TempDir(), t.TempDir()
	clustertrusttest.WriteKeypair(t, selfDir, "uuid-self")
	clustertrusttest.WriteKeypair(t, peerDir, principal)
	clustertrusttest.WriteAdmission(t, selfDir, "cluster-test", 1)
	clustertrusttest.WriteAdmission(t, peerDir, "cluster-test", 1)
	writePinFromCert(t, selfDir, principal, filepath.Join(peerDir, "node.crt"))
	writePinFromCert(t, peerDir, "uuid-self", filepath.Join(selfDir, "node.crt"))
	mesh := clustertrust.Open(peerDir)
	d := newSelfTestDaemon("uuid-self", "192.0.2.1")
	d.mesh = clustertrust.Open(selfDir)
	d.clusterDir = selfDir
	d.modelsClients = clustertrust.NewPeerClientPool(d.mesh, modelsFetchTimeout)
	mismatch := &atomic.Bool{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := mesh.VerifyClientPin(r); !ok {
			http.Error(w, "forbidden", 403)
			return
		}
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string][]string{"models": {"authenticated-model"}})
			return
		}
		if r.URL.Path == "/v1/engine-stats" {
			body := engineNativeBody()
			body["observedAt"] = 1000
			_ = json.NewEncoder(w).Encode(body)
			return
		}
		_, p, _ := net.SplitHostPort(r.Host)
		port, _ := strconv.Atoi(p)
		host := principal
		if mismatch.Load() {
			host = "other-peer"
		}
		info := NodeInfoResponse{HostUUID: host, ClusterUUID: &principal, TelemetryValid: true, SampleSeq: 12, GPUs: []GPUInfo{{Name: "authenticated-gpu", UtilizationPercent: gpuReading(uint32(43))}}, PeerMetadata: &noderec.PeerMetadataParams{HostUUID: host, Name: "authenticated-name", Services: map[noderec.ServiceKey]int{noderec.ServiceNodeInfo: port, noderec.ServiceEngineManager: port, noderec.ServiceOllama: 11434}}}
		_ = json.NewEncoder(w).Encode(info)
	}))
	srv.TLS = mesh.ServerTLSConfig()
	srv.StartTLS()
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		d.modelsClients.CloseIdle()
		if d.peerInfoClients != nil {
			d.peerInfoClients.CloseIdle()
		}
	})
	return d, srv, mismatch
}

func TestPublicPreviewHasIndependentKeyAndCannotRemoveVerifiedPeer(t *testing.T) {
	d, srv, _ := secureInventoryFixture(t, "uuid-peer")
	host, p, _ := net.SplitHostPort(srv.Listener.Addr().String())
	genuine := RawNode{ID: "genuine", Addresses: []string{host}, TXT: []string{"v=1", "uuid=uuid-peer", "cluster-uuid=uuid-peer", "ip=" + host, "ni=" + p}}
	d.onBrowse(DiscoveryEvent{Type: "discovered", Node: genuine})
	before, exists := d.dir.get("uuid-peer")
	if !exists || !before.ServicePeer() {
		t.Fatal("verified fixture not promoted")
	}
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(NodeInfoResponse{HostUUID: "uuid-peer", GPUs: []GPUInfo{{Name: "public-preview"}}})
	}))
	defer public.Close()
	ph, pp, _ := net.SplitHostPort(public.Listener.Addr().String())
	hint := RawNode{ID: "forged-preview", Addresses: []string{ph}, TXT: []string{"v=1", "uuid=uuid-peer", "ip=" + ph, "ni=" + pp, "ol=1"}}
	d.onBrowse(DiscoveryEvent{Type: "discovered", Node: hint})
	key := candidateRecordKey(hint)
	preview, exists := d.dir.get(key)
	if !exists || preview.RecordKey() != key || preview.IdentityVerification != "unverified" || preview.Trusted {
		t.Fatalf("preview not independently keyed: %+v", preview)
	}
	if after, _ := d.dir.get("uuid-peer"); after.PeerRecordID != before.PeerRecordID || !after.ServicePeer() {
		t.Fatal("preview displaced verified identity")
	}
	d.onBrowse(DiscoveryEvent{Type: "removed", Node: hint})
	if _, exists := d.dir.get(key); exists {
		t.Fatal("preview removal left its own row")
	}
	if after, _ := d.dir.get("uuid-peer"); after.PeerRecordID != before.PeerRecordID || !after.ServicePeer() {
		t.Fatal("preview removal withdrew verified inventory")
	}
}

func TestPinnedInventoryPromotesOnlyAuthenticatedMetadata(t *testing.T) {
	d, srv, mismatch := secureInventoryFixture(t, "uuid-peer")
	host, p, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(p)
	raw := RawNode{ID: "forged-name", Addresses: []string{host}, TXT: []string{"v=1", "uuid=uuid-peer", "cluster-uuid=uuid-peer", "ip=" + host, "ni=" + p, "em=1", "ol=1"}}
	d.onBrowse(DiscoveryEvent{Type: "discovered", Node: raw})
	n, ok := d.dir.get("uuid-peer")
	if !ok || !n.ServicePeer() || n.PeerRecordID == "" || n.Name != "authenticated-name" || n.Services[noderec.ServiceOllama].Port != 11434 || len(n.Models) != 1 || n.Models[0] != "authenticated-model" {
		t.Fatalf("authenticated projection = %+v", n)
	}
	var out bytes.Buffer
	d.codec = NewCodec(&out)
	if !d.refreshNodeTelemetryCandidates(context.Background(), n.HostUUID, []string{host}, port) {
		t.Fatal("pinned telemetry rejected")
	}
	var message Message
	_ = json.Unmarshal(out.Bytes(), &message)
	var telemetry noderec.NodeTelemetry
	_ = json.Unmarshal(message.Params, &telemetry)
	if !telemetry.TelemetryValid || telemetry.PeerRecordID != n.PeerRecordID || telemetry.GPUUtilizationPct != 43 {
		t.Fatalf("authenticated telemetry = %+v", telemetry)
	}
	mismatch.Store(true)
	if d.refreshNodeTelemetryCandidates(context.Background(), n.HostUUID, []string{host}, port) {
		t.Fatal("mismatched authenticated response identity accepted")
	}
	mismatch.Store(false)
	// Same certificate, different local admission: old proofs are not reusable.
	clustertrusttest.WriteAdmission(t, d.clusterDir, "cluster-test", 2)
	d.reloadTrust()
	n, _ = d.dir.get("uuid-peer")
	if n.Trusted || n.ServicePeer() || len(n.Models) > 0 || len(n.GPUs) > 0 {
		t.Fatalf("admission change retained inventory: %+v", n)
	}
}
