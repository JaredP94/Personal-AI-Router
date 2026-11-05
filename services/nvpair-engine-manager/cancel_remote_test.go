// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"nvpair-shared/noderec"
)

func TestRemoteCancelRequiresVerifiedPeer(t *testing.T) {
	for _, verification := range []string{"unverified", "manual", ""} {
		t.Run(verification, func(t *testing.T) {
			var out bytes.Buffer
			ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
			srv, mesh := newToolControlPeer(t, ex, true)
			m := NewManager(NewCodec(&out), ex, mesh)
			defer m.remoteHTTP.CloseIdle()
			defer m.readyHTTP.CloseIdle()
			defer m.settingsHTTP.CloseIdle()
			m.peers.set([]noderec.DirectoryNode{{HostUUID: "claimed", ClusterUUID: "uuid-server", IP: "127.0.0.1", Trusted: true, PeerRecordID: "preview:claimed", IdentityVerification: verification, Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceEngineControl: {Port: listenerPort(t, srv)}}}})
			for _, method := range []string{"engine:remote-pull-model", "engine:remote-cancel-pull"} {
				out.Reset()
				id := json.RawMessage("1")
				m.runRemote(context.Background(), &Message{ID: &id, Method: method, Params: json.RawMessage(`{"node":"preview:claimed","engine":"ollama","model":"a","opId":"one"}`)})
				if !strings.Contains(out.String(), "verified peer") {
					t.Fatalf("unverified request was not refused: %s", out.String())
				}
			}
		})
	}
}

func TestRemoteCancelAcceptedBeforeAdapterConfirmation(t *testing.T) {
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	started, stopping, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_, _ = ex.trackedPull(context.Background(), "ollama", "a", "one", true, func(ctx context.Context) (json.RawMessage, error) {
			close(started)
			<-ctx.Done()
			close(stopping)
			<-release
			return nil, ctx.Err()
		})
	}()
	<-started
	srv, mesh := newToolControlPeer(t, ex, true)
	response := callToolRemote(t, srv, mesh, "engine:remote-cancel-pull", `{"node":"host-server","engine":"ollama","model":"a","opId":"one"}`)
	if response.Error != nil || !strings.Contains(string(response.Result), `"accepted":true`) {
		t.Fatalf("cancel not accepted: %+v", response)
	}
	select {
	case <-stopping:
	case <-time.After(time.Second):
		t.Fatal("stop not delivered")
	}
	select {
	case <-done:
		t.Fatal("acceptance waited for or invented stop")
	default:
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("no confirmation")
	}
}

func TestCancelRouteRefusesUnpinnedCaller(t *testing.T) {
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	srv, mesh := newToolControlPeer(t, ex, false)
	cfg, ok := mesh.ClientTLSConfig("uuid-server")
	if !ok {
		t.Fatal("server pin missing")
	}
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
	defer client.CloseIdleConnections()
	res, err := client.Post(srv.URL+controlCancelPullPath, "application/json", strings.NewReader(`{"engine":"ollama","model":"a","opId":"one"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("unpinned cancellation HTTP %d", res.StatusCode)
	}
}
