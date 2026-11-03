// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Cross-process model enrichment through pinned cluster mTLS: discovery first
// proves the peer's node-info identity and service map, then fetches /v1/models
// from that authenticated engine-manager port. An unauthenticated lookalike
// remains a preview without service inventory.
package tests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grandcat/zeroconf"
)

func TestModelsHTTPEnrichment(t *testing.T) {
	requireMulticast(t)
	// Stand-in engine-manager inventory, served only after pinned mTLS proof.
	models := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		// Flat union + per-engine attribution, exactly the shape engine-manager's
		// ModelsResult serializes. The daemon must enrich both onto the node.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []string{"llama3:8b", "qwen:0.5b"},
			"modelsByEngine": map[string][]string{
				"ollama":   {"llama3:8b"},
				"lmstudio": {"qwen:0.5b"},
			},
			"loadedByEngine": map[string][]string{
				"ollama":   {"llama3:8b"},
				"lmstudio": {},
			},
		})
	})
	const instance = "xproc-models-1"
	fx := newInterNodeClusterWithPeer(t, instance+"-uuid")
	fx.startVerifiedPeer(t, instance, models, nil)

	// This lookalike has only public HTTP and a claimed principal, not the key.
	var unprovenModelReads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			unprovenModelReads.Add(1)
		}
		models.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse stub url: %v", err)
	}
	emPort, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("stub port: %v", err)
	}

	const preview = "xproc-models-preview"
	txt := []string{"v=1", "uuid=" + fx.peerUUID, "cluster-uuid=" + fx.peerUUID, "ip=127.0.0.1", fmt.Sprintf("ni=%d", emPort), fmt.Sprintf("em=%d", emPort)}
	zsrv, err := zeroconf.Register(preview, nodeRecordService, testDomain, emPort, txt, nil)
	if err != nil {
		t.Fatalf("register %s: %v", preview, err)
	}
	t.Cleanup(zsrv.Shutdown)
	stdin, msgs, cleanup := startBrokerProcInCluster(t, fx.nodeDir, "--scanner-path", scannerBin)
	t.Cleanup(cleanup)

	pollForNode(t, stdin, msgs, preview, 30*time.Second, func(n availableNode) bool {
		if n.IdentityVerification != "unverified" || n.Trusted || len(n.Models) != 0 || unprovenModelReads.Load() != 0 {
			t.Fatalf("unauthenticated stub supplied service inventory: %+v, model reads=%d", n, unprovenModelReads.Load())
		}
		return true
	})

	// Poll get-nodes until the node appears carrying the enriched model list.
	deadline := time.After(30 * time.Second)
	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()
	id := 100
	sendReq(t, stdin, id, "discovery:get-nodes")
	for {
		select {
		case msg, ok := <-msgs:
			if !ok {
				t.Fatal("broker stream closed unexpectedly")
			}
			if msg.Method == "" && msg.ID != nil {
				var res availableNodesResult
				if json.Unmarshal(msg.Result, &res) == nil {
					if n, found := findNode(res.Nodes, instance); found &&
						n.IdentityVerification == "verified" && n.Trusted && n.HostUUID == fx.peerUUID &&
						modelsMatch(n.Models) && modelsByEngineMatch(n.ModelsByEngine) &&
						loadedByEngineMatch(n.LoadedByEngine) {
						if unprovenModelReads.Load() != 0 {
							t.Fatal("discovery fetched model inventory from the unauthenticated stub")
						}
						return // enriched flat + per-engine + loaded lists surfaced
					}
				}
			}
		case <-ticker.C:
			id++
			sendReq(t, stdin, id, "discovery:get-nodes")
		case <-deadline:
			t.Fatalf("timed out waiting for %q with an enriched model list", instance)
		}
	}
}

func findNode(nodes []availableNode, id string) (availableNode, bool) {
	for _, n := range nodes {
		if n.ID == id {
			return n, true
		}
	}
	return availableNode{}, false
}

func modelsMatch(models []string) bool {
	want := []string{"llama3:8b", "qwen:0.5b"}
	if len(models) != len(want) {
		return false
	}
	for i := range want {
		if models[i] != want[i] {
			return false
		}
	}
	return true
}

func modelsByEngineMatch(byEngine map[string][]string) bool {
	want := map[string][]string{
		"ollama":   {"llama3:8b"},
		"lmstudio": {"qwen:0.5b"},
	}
	return byEngineEqual(byEngine, want)
}

// loadedByEngineMatch asserts the loaded set the stub served (ollama has one
// resident model; lmstudio is running but empty) survives the daemon->broker
// projection onto the client-facing node.
func loadedByEngineMatch(loaded map[string][]string) bool {
	want := map[string][]string{
		"ollama":   {"llama3:8b"},
		"lmstudio": {},
	}
	return byEngineEqual(loaded, want)
}

// byEngineEqual reports whether a per-engine map equals want: same key set, each
// with the same ordered list (an empty list and a present-but-empty key both
// count, so "running, nothing loaded" is distinguishable from a missing key).
func byEngineEqual(got, want map[string][]string) bool {
	if len(got) != len(want) {
		return false
	}
	for engine, wantList := range want {
		gotList, ok := got[engine]
		if !ok || len(gotList) != len(wantList) {
			return false
		}
		for i := range wantList {
			if gotList[i] != wantList[i] {
				return false
			}
		}
	}
	return true
}
