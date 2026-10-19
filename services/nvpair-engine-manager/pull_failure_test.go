// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nvpair-shared/clustertrust"
	"nvpair-shared/noderec"
)

// HTTP 200 does not mean a pull succeeded: Ollama sends failures in NDJSON.
func TestPullEngineErrorBecomesRPCError(t *testing.T) {
	for _, path := range []string{"local", "peer-endpoint", "remote-wrapper"} {
		t.Run(path, func(t *testing.T) {
			engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/api/pull" {
					t.Errorf("unexpected engine request: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/x-ndjson")
				_, _ = w.Write([]byte("{\"status\":\"pulling manifest\"}\n{\"error\":\"model does not exist\"}\n"))
			}))
			defer engine.Close()
			manifest := testEngineManifest(fakeEngineBin)
			manifest.Actions["pull_model"] = Action{HTTP: &ActionHTTP{Method: "POST", Path: "/api/pull"}}
			ex := newTestExecutor(t, manifest)
			st, err := ex.state("fake")
			if err != nil {
				t.Fatal(err)
			}
			st.running, st.port = true, portOf(t, engine.URL)
			id := json.RawMessage("42")
			var out bytes.Buffer
			switch path {
			case "local":
				mgr := NewManager(NewCodec(&out), ex, nil)
				mgr.runAction(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: "engine:action",
					Params: json.RawMessage(`{"engine":"fake","action":"pull_model","params":{"name":"missing-model"}}`)})
			case "peer-endpoint":
				rec := httptest.NewRecorder()
				(&controlServer{exec: ex}).handlePull(rec, httptest.NewRequest("POST", controlPullPath,
					strings.NewReader(`{"opId":"failure","engine":"fake","model":"missing-model"}`)))
				dec := json.NewDecoder(rec.Body)
				var terminal streamFrame
				for dec.More() {
					terminal = streamFrame{}
					if err := dec.Decode(&terminal); err != nil {
						t.Fatal(err)
					}
					if terminal.Type == "result" {
						t.Fatalf("failed pull emitted result: %+v", terminal)
					}
				}
				if terminal.Type != "error" || !strings.Contains(terminal.Message, "model does not exist") {
					t.Fatalf("expected terminal pull error, got %+v", terminal)
				}
				return
			case "remote-wrapper":
				// Exercise the real caller and peer endpoint over pinned mTLS.
				peerCert, peerKey := mintLeaf(t, "pull-peer")
				selfCert, selfKey := mintLeaf(t, "pull-self")
				peerMesh := clustertrust.Open(clusterDirFor(t, peerCert, peerKey, map[string][]byte{"pull-self": selfCert}))
				selfMesh := clustertrust.Open(clusterDirFor(t, selfCert, selfKey, map[string][]byte{"pull-peer": peerCert}))
				srv := httptest.NewUnstartedServer(requirePinnedPeer(peerMesh, http.HandlerFunc((&controlServer{exec: ex}).handlePull)))
				srv.TLS = peerMesh.ServerTLSConfig()
				srv.StartTLS()
				defer srv.Close()
				mgr := NewManager(NewCodec(&out), ex, selfMesh)
				mgr.peers.set([]noderec.DirectoryNode{{HostUUID: "host-peer", ClusterUUID: "pull-peer", IP: "127.0.0.1",
					Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceEngineControl: {Port: portOf(t, srv.URL)}}}})
				mgr.runRemote(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: "engine:remote-pull-model",
					Params: json.RawMessage(`{"node":"host-peer","engine":"fake","model":"missing-model"}`)})
			}
			dec := json.NewDecoder(&out)
			var response Message
			for dec.More() {
				response = Message{}
				if err := dec.Decode(&response); err != nil {
					t.Fatal(err)
				}
				if response.ID != nil {
					break
				}
			}
			if response.Error == nil || !strings.Contains(response.Error.Message, "model does not exist") || len(response.Result) != 0 {
				t.Fatalf("failed pull must be a JSON-RPC error, got %+v", response)
			}
		})
	}
}
