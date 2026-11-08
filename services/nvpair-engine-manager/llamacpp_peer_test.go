// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nvpair-shared/clustertrust"
)

func TestLlamaPeerInventoryIncludesStoppedCache(t *testing.T) {
	ex, cache := llamaDownloadExecutor(t, nil, nil)
	if err := pullLlamaTest(ex, "peer-inventory"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "manual.gguf"), llamaTestBytes(), 0600); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	modelsHandler(ex)(rec, httptest.NewRequest(http.MethodGet, modelsPath, nil))
	var body struct {
		Downloaded map[string][]struct {
			ID    string `json:"id"`
			File  string `json:"file"`
			Size  int64  `json:"size"`
			Owned bool   `json:"owned"`
		} `json:"downloadedByEngine"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	rows := body.Downloaded["llamacpp"]
	if len(rows) != 2 || rows[0].ID != llamaTestID || rows[0].File != llamaOwnedFilename(llamaTestID) || rows[0].Size != int64(len(llamaTestBytes())) || !rows[0].Owned || rows[1].ID != "manual.gguf" || rows[1].Owned {
		t.Fatalf("cache inventory: %s", rec.Body.String())
	}
	if len(ex.Models(context.Background())) != 0 {
		t.Fatal("stopped cache leaked into served routing inventory")
	}
}

func TestLlamaPeerInventoryBeforeCacheParentExists(t *testing.T) {
	ex, _ := llamaDownloadExecutor(t, nil, nil)
	state, err := ex.state("llamacpp")
	if err != nil {
		t.Fatal(err)
	}
	state.installDir = filepath.Join(t.TempDir(), "not-created", "engine-bin", "llamacpp")
	rec := httptest.NewRecorder()
	modelsHandler(ex)(rec, httptest.NewRequest(http.MethodGet, modelsPath, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"downloadedByEngine":{"llamacpp":[]}`) {
		t.Fatalf("fresh cache unavailable: %d %s", rec.Code, rec.Body.String())
	}
}

func TestLlamaCacheFailurePreservesOtherEngineInventory(t *testing.T) {
	ex, cache := llamaDownloadExecutor(t, nil, nil)
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, llamaManifestName), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	mf := testEngineManifest(fakeEngineForTest(t))
	mf.Actions["list_models"] = Action{HTTP: &ActionHTTP{Method: "GET", Path: "/api/tags"}, Result: &ActionResult{Array: "models", Field: "name"}}
	ex.reg.engines["fake"] = mf
	reconciledTestExecutor(ex)
	t.Cleanup(func() { _ = ex.Stop("fake") })
	if err := ex.Start(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	result, err := ex.modelsResult(context.Background())
	if len(result.ByEngine["fake"]) != 1 || result.ByEngine["fake"][0] != "llama3.2:1b" {
		t.Fatalf("cache failure erased served inventory: %+v %v", result, err)
	}
	selfCert, selfKey := mintLeaf(t, "inventory-owner")
	peerCert, peerKey := mintLeaf(t, "inventory-caller")
	owner := clustertrust.Open(clusterDirFor(t, selfCert, selfKey, map[string][]byte{"inventory-caller": peerCert}))
	caller := clustertrust.Open(clusterDirFor(t, peerCert, peerKey, map[string][]byte{"inventory-owner": selfCert}))
	_, mux := emSurface(ex, owner)
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = owner.ServerTLSConfig()
	srv.StartTLS()
	defer srv.Close()
	cfg, _ := caller.ClientTLSConfig("inventory-owner")
	transport := &http.Transport{TLSClientConfig: cfg}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	resp, err := client.Get(srv.URL + modelsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Models []string          `json:"models"`
		Errors map[string]string `json:"downloadErrorsByEngine"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || resp.StatusCode != http.StatusOK || len(body.Models) != 1 || body.Errors["llamacpp"] != "cache_unavailable" || len(body.Errors) != 1 {
		t.Fatalf("cache error failed pinned HTTP inventory: status=%d body=%+v err=%v", resp.StatusCode, body, err)
	}
}

func TestLlamaRemoteDeleteUsesIdentityAndRunningGuard(t *testing.T) {
	for _, mode := range []string{"delete", "running", "replacement", "unpinned"} {
		t.Run(mode, func(t *testing.T) {
			ex, cache := llamaDownloadExecutor(t, nil, nil)
			if err := pullLlamaTest(ex, "peer-delete"); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(cache, llamaOwnedFilename(llamaTestID))
			if mode == "running" {
				st, _ := ex.state("llamacpp")
				st.mu.Lock()
				st.running = true
				st.mu.Unlock()
			}
			if mode == "replacement" {
				if err := os.Rename(file, file+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, llamaTestBytes(), 0600); err != nil {
					t.Fatal(err)
				}
			}
			selfCert, selfKey := mintLeaf(t, "delete-owner")
			peerCert, peerKey := mintLeaf(t, "delete-caller")
			pins := map[string][]byte{"delete-caller": peerCert}
			if mode == "unpinned" {
				pins = nil
			}
			owner := clustertrust.Open(clusterDirFor(t, selfCert, selfKey, pins))
			caller := clustertrust.Open(clusterDirFor(t, peerCert, peerKey, map[string][]byte{"delete-owner": selfCert}))
			server := &controlServer{exec: ex, mesh: owner}
			srv := httptest.NewUnstartedServer(server.mux())
			srv.TLS = owner.ServerTLSConfig()
			srv.StartTLS()
			defer srv.Close()
			cfg, ok := caller.ClientTLSConfig("delete-owner")
			if !ok {
				t.Fatal("missing peer pin")
			}
			transport := &http.Transport{TLSClientConfig: cfg}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport}
			remote := &remoteClient{http: client, readyHTTP: client, base: srv.URL}
			result, err := remote.postJSON(context.Background(), controlDeletePath, "llamacpp", modelActionRequest{Engine: "llamacpp", Model: llamaTestID})
			_, statErr := os.Stat(file)
			if mode == "delete" {
				if err != nil || !os.IsNotExist(statErr) {
					t.Fatalf("delete: %s %v %v", result, err, statErr)
				}
			} else if err == nil || statErr != nil {
				t.Fatalf("unsafe delete: %s %v %v", result, err, statErr)
			}
			if mode == "running" && !strings.Contains(err.Error(), "stop the engine") {
				t.Fatalf("missing refusal reason: %v", err)
			}
			if mode == "unpinned" && !strings.Contains(err.Error(), "403") {
				t.Fatalf("missing peer refusal: %v", err)
			}
		})
	}
}
