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
	"testing"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/noderec"
)

// callToolRemote goes through the manager's async dispatch and the actual ec
// mTLS stream, rather than bypassing dispatch by calling runRemote directly.
func callToolRemote(t *testing.T, srv *httptest.Server, mesh *clustertrust.Mesh, method, params string) *Message {
	t.Helper()
	managerConn, clientConn := net.Pipe()
	defer managerConn.Close()
	defer clientConn.Close()
	if err := clientConn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	m := NewManager(NewCodec(managerConn), &Executor{progress: newProgressHub()}, mesh)
	defer m.remoteHTTP.CloseIdle()
	defer m.readyHTTP.CloseIdle()
	m.peers.set([]noderec.DirectoryNode{{
		HostUUID: "host-server", Name: "server", IP: "127.0.0.1", ClusterUUID: "uuid-server", IdentityVerification: "verified", Trusted: true, PeerRecordID: "server-record",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceEngineControl: {Port: listenerPort(t, srv)},
		},
	}})
	id := json.RawMessage("1")
	returned := make(chan struct{})
	go func() {
		m.handleMessage(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: method, Params: json.RawMessage(params)})
		close(returned)
	}()
	dec := json.NewDecoder(clientConn)
	var response Message
	if err := dec.Decode(&response); err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("remote dispatch did not return")
	}
	if response.ID == nil || string(*response.ID) != "1" {
		t.Fatalf("unexpected response: %+v", response)
	}
	return &response
}

func TestRemoteInstallToolPreservesErrorCode(t *testing.T) {
	for _, tc := range []struct {
		name, platform string
		brewFails      bool
		code           int
		message        string
	}{
		{name: "Homebrew missing", code: -32010, message: "Homebrew was not found"},
		{name: "unsupported platform", platform: "linux/amd64", code: -32011, message: "tool macmon is not supported on linux/amd64"},
		{name: "install failed", brewFails: true, code: -32012, message: "install tool macmon: exit status 1: brew failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex, brew, _, _ := newToolTestExecutor(t)
			if tc.platform != "" {
				ex.tools.Platform = tc.platform
			}
			if tc.brewFails {
				writeToolFile(t, brew, "#!/bin/sh\nprintf 'brew failed' >&2\nexit 1\n", 0o755)
			}
			srv, mesh := newToolControlPeer(t, ex, true)
			response := callToolRemote(t, srv, mesh, "engine:remote-install-tool", `{"node":"host-server","tool":"macmon"}`)
			if response.Error == nil || response.Error.Code != tc.code || response.Error.Message != tc.message {
				t.Fatalf("error = %+v, want code %d, message %q", response.Error, tc.code, tc.message)
			}
		})
	}
}

func TestRemoteInstallToolResult(t *testing.T) {
	ex, _, detect, _ := newToolTestExecutor(t)
	writeToolFile(t, detect, "", 0o755)
	srv, mesh := newToolControlPeer(t, ex, true)
	response := callToolRemote(t, srv, mesh, "engine:remote-install-tool", `{"node":"host-server","tool":"macmon"}`)
	var result struct {
		OpID   string `json:"opId"`
		Result struct {
			Tool      string `json:"tool"`
			Installed bool   `json:"installed"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatalf("result = %s, error = %+v: %v", response.Result, response.Error, err)
	}
	if response.Error != nil || result.OpID == "" || result.Result.Tool != "macmon" || !result.Result.Installed {
		t.Fatalf("unexpected success response: %+v, result: %+v", response, result)
	}
}

func TestRemoteInstallToolRequiresTool(t *testing.T) {
	for _, params := range []string{`{"node":"host-server"}`, `{"node":"host-server","tool":""}`} {
		ex, _, _, _ := newToolTestExecutor(t)
		srv, mesh := newToolControlPeer(t, ex, true)
		response := callToolRemote(t, srv, mesh, "engine:remote-install-tool", params)
		if response.Error == nil || response.Error.Code != -32602 || response.Error.Message != "tool is required" {
			t.Fatalf("error = %+v, want -32602: tool is required", response.Error)
		}
	}
}

func TestRemoteStreamErrorWithoutCodeMapsToServerError(t *testing.T) {
	for _, tc := range []struct {
		method, params, message string
	}{
		{"engine:remote-install-tool", `{"node":"host-server","tool":"rm"}`, `unknown tool "rm"`},
		{"engine:remote-install", `{"node":"host-server","engine":"absent"}`, `unknown engine "absent"`},
	} {
		t.Run(tc.method, func(t *testing.T) {
			ex, _, _, _ := newToolTestExecutor(t)
			srv, mesh := newToolControlPeer(t, ex, true)
			response := callToolRemote(t, srv, mesh, tc.method, tc.params)
			if response.Error == nil || response.Error.Code != -32000 || response.Error.Message != tc.message {
				t.Fatalf("error = %+v, want -32000 and unchanged message %q", response.Error, tc.message)
			}
		})
	}
}

func TestRemoteDownloadInterruptedOnPeerWorkerExitOrInventoryLoss(t *testing.T) {
	for _, loss := range []string{"worker exit", "inventory loss"} {
		t.Run(loss, func(t *testing.T) {
			ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
			srv, mesh := newToolControlPeer(t, ex, true)
			srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				_, _ = w.Write([]byte(`{"type":"progress","engine":"ollama","model":"model","op":"pull","stage":"downloading"}` + "\n"))
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			})
			managerConn, clientConn := net.Pipe()
			defer managerConn.Close()
			defer clientConn.Close()
			_ = clientConn.SetDeadline(time.Now().Add(5 * time.Second))
			m := NewManager(NewCodec(managerConn), ex, mesh)
			defer m.remoteHTTP.CloseIdle()
			defer m.readyHTTP.CloseIdle()
			m.updateDownloadPeers([]noderec.DirectoryNode{{HostUUID: "host-server", IP: "127.0.0.1", ClusterUUID: "uuid-server", IdentityVerification: "verified", Trusted: true, PeerRecordID: "record", Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceEngineControl: {Port: listenerPort(t, srv)}}}})
			id := json.RawMessage("1")
			done := make(chan struct{})
			go func() {
				defer close(done)
				m.runRemote(context.Background(), &Message{ID: &id, Method: "engine:remote-pull-model", Params: json.RawMessage(`{"node":"host-server","engine":"ollama","model":"model","opId":"pull"}`)})
			}()
			codec := NewCodec(clientConn)
			first, err := codec.Read()
			if err != nil || first.Method != "engine:remote-progress" {
				t.Fatalf("first progress: %v %v", first, err)
			}
			if loss == "worker exit" {
				srv.CloseClientConnections()
			} else {
				m.updateDownloadPeers(nil)
			}
			terminal, err := codec.Read()
			if err != nil {
				t.Fatal(err)
			}
			var p remoteProgress
			if json.Unmarshal(terminal.Params, &p) != nil || p.Stage != "interrupted" || p.OpID != "pull" {
				t.Fatalf("not interrupted: %s", terminal.Params)
			}
			response, err := codec.Read()
			if err != nil || response.Error == nil {
				t.Fatalf("response: %v %v", response, err)
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("pull did not return")
			}
		})
	}
}

func TestRunRemoteRequiresNode(t *testing.T) {
	var out bytes.Buffer
	m := NewManager(NewCodec(&out), &Executor{progress: newProgressHub()}, nil)
	id := json.RawMessage("1")
	m.runRemote(context.Background(), &Message{JSONRPC: "2.0", ID: &id,
		Method: "engine:remote-install", Params: json.RawMessage(`{"engine":"ollama"}`)})
	mustContain(t, out.String(), "node is required")
}

func TestRunRemoteUnknownPeer(t *testing.T) {
	var out bytes.Buffer
	m := NewManager(NewCodec(&out), &Executor{progress: newProgressHub()}, nil)
	id := json.RawMessage("1")
	m.runRemote(context.Background(), &Message{JSONRPC: "2.0", ID: &id,
		Method: "engine:remote-install", Params: json.RawMessage(`{"node":"ghost","engine":"ollama"}`)})
	mustContain(t, out.String(), "not a discovered ec peer")
}

func TestRunRemoteNotClustered(t *testing.T) {
	var out bytes.Buffer
	// Peer is discovered, but this node has no mesh (not clustered), so the
	// pinned dial can't be built.
	m := NewManager(NewCodec(&out), &Executor{progress: newProgressHub()}, nil)
	m.peers.set([]noderec.DirectoryNode{{
		HostUUID: "uuid-b", Name: "nodeB", IP: "192.168.1.42", ClusterUUID: "cuuid-b", IdentityVerification: "verified", Trusted: true, PeerRecordID: "b-record",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceEngineControl: {Port: 14323},
		},
	}})
	id := json.RawMessage("1")
	m.runRemote(context.Background(), &Message{JSONRPC: "2.0", ID: &id,
		Method: "engine:remote-install", Params: json.RawMessage(`{"node":"uuid-b","engine":"ollama"}`)})
	mustContain(t, out.String(), "not clustered")
}
