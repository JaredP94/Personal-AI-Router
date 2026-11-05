// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"nvpair-shared/clustertrust"
	settings "nvpair-shared/enginesettings"
)

func TestLoadProfileVerifiedPeerEditAndLoad(t *testing.T) {
	a, certA := settingsMesh(t, "profile-target")
	b, certB := settingsMesh(t, "profile-caller")
	stranger, _ := settingsMesh(t, "profile-stranger")
	settingsPin(t, a, "profile-caller", certB)
	for _, dir := range []string{b, stranger} {
		settingsPin(t, dir, "profile-target", certA)
	}
	mesh := clustertrust.Open(a)
	e, capture, _ := profileActionExecutor(t)
	server := httptest.NewUnstartedServer((&controlServer{exec: e, mesh: mesh}).mux())
	server.TLS = mesh.ServerTLSConfig()
	server.StartTLS()
	defer server.Close()
	client := func(dir string) *http.Client {
		config, ok := clustertrust.Open(dir).ClientTLSConfig("profile-target")
		if !ok {
			t.Fatal("missing pin")
		}
		transport := &http.Transport{TLSClientConfig: config}
		t.Cleanup(transport.CloseIdleConnections)
		return &http.Client{Transport: transport, Timeout: 3 * time.Second}
	}
	paired := client(b)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	post := func(client *http.Client, path string, p any) *http.Response {
		t.Helper()
		data, _ := json.Marshal(p)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+path, bytes.NewReader(data))
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { response.Body.Close() })
		return response
	}
	request := settings.Request{Engine: "lmstudio", Model: "keep", RequestID: "peer-one", LoadText: "--gpu max", NodeID: "must-not-forward"}
	response := post(paired, settingsPath+"apply-load-profile", request)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("paired apply status %d", response.StatusCode)
	}
	var snap settings.LoadProfileSnapshot
	if json.NewDecoder(response.Body).Decode(&snap) != nil || snap.LoadText != "--gpu max" || snap.NodeID != "" {
		t.Fatal("invalid peer snapshot")
	}
	response = post(paired, controlLoadPath, modelActionRequest{Engine: "lmstudio", Model: "keep"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("paired load status %d", response.StatusCode)
	}
	if args := capturedProfileLoads(t, capture); len(args) != 1 || args[0][2] != "--identifier" || args[0][3] != "keep" || args[0][4] != "--gpu" || args[0][5] != "max" {
		t.Fatal("remote load omitted saved options")
	}
	// A saved identity claim is insufficient without current mutual trust.
	request.RequestID = "peer-two"
	request.ExpectedRevision = snap.Revision
	data, _ := json.Marshal(request)
	unknownRequest, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+settingsPath+"apply-load-profile", bytes.NewReader(data))
	if response, err := client(stranger).Do(unknownRequest); err == nil {
		response.Body.Close()
		if response.StatusCode == http.StatusOK {
			t.Fatal("unverified peer edit accepted")
		}
	}
	if err := os.Remove(filepath.Join(a, "trusted", "profile-caller.json")); err != nil {
		t.Fatal(err)
	}
	mesh.Refresh()
	revokedRequest, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+settingsPath+"apply-load-profile", bytes.NewReader(data))
	if response, err := paired.Do(revokedRequest); err == nil {
		response.Body.Close()
		if response.StatusCode == http.StatusOK {
			t.Fatal("revoked peer edit accepted")
		}
	}
}
