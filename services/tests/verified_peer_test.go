// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"

	"github.com/grandcat/zeroconf"

	"nvpair-shared/clustertrust"
	"nvpair-shared/noderec"
)

// startVerifiedPeer uses the same synthetic admission and exact cross-pins as
// the inter-node data-plane tests. Discovery must prove this peer through a
// real mTLS node-info read before consuming its service map. All inventory and
// workload requests also require the broker's pinned client leaf.
// The returned stop withdraws both the advertisement and the answering server.
func (c *interNodeCluster) startVerifiedPeer(t *testing.T, instance string, models, workloads http.Handler) func() {
	t.Helper()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("verified peer listen: %v", err)
	}
	return c.startVerifiedPeerOn(t, instance, models, workloads, ln)
}

// startVerifiedPeerOn lets a wipe regression reuse the exact listener address
// after stopping the original, authenticated incarnation.
func (c *interNodeCluster) startVerifiedPeerOn(t *testing.T, instance string, models, workloads http.Handler, ln net.Listener) func() {
	t.Helper()
	_, portString, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portString)
	if err != nil {
		t.Fatal(err)
	}
	services := map[noderec.ServiceKey]int{noderec.ServiceNodeInfo: port}
	txt := []string{"v=1", "uuid=" + c.peerUUID, "ip=127.0.0.1", fmt.Sprintf("ni=%d", port), clustertrust.ClusterUUIDTXTKey + "=" + c.peerUUID}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/node-info", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"hostUuid": c.peerUUID, "clusterUuid": c.peerUUID,
			"peerMetadata": noderec.PeerMetadataParams{HostUUID: c.peerUUID, Name: instance, Services: services},
			"GPUs":         []any{}, "telemetryValid": false,
		})
	})
	if models != nil {
		services[noderec.ServiceEngineManager] = port
		txt = append(txt, fmt.Sprintf("em=%d", port))
		mux.Handle("/v1/models", models)
	}
	if workloads != nil {
		services[noderec.ServiceWorkload] = port
		txt = append(txt, fmt.Sprintf("wl=%d", port))
		mux.Handle("/v1/workloads/events", workloads)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := c.peer.VerifyClientPin(r); !ok {
			http.Error(w, "not a pinned peer", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})}
	go func() { _ = srv.Serve(tls.NewListener(ln, c.peer.ServerTLSConfig())) }()
	t.Cleanup(func() { _ = srv.Close() })
	adv, err := zeroconf.Register(instance, nodeRecordService, testDomain, port, txt, nil)
	if err != nil {
		t.Fatalf("advertise verified peer: %v", err)
	}
	var once sync.Once
	stop := func() { once.Do(func() { adv.Shutdown(); _ = srv.Close() }) }
	t.Cleanup(stop)
	t.Logf("advertising pinned peer %s (principal=%s, ni=%d)", instance, c.peerUUID, port)
	return stop
}
