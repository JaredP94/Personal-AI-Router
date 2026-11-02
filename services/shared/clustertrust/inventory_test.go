// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clustertrust

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"nvpair-shared/clustertrusttest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func inventoryPair(t *testing.T) (string, *Mesh, *Mesh) {
	t.Helper()
	selfDir, peerDir := t.TempDir(), t.TempDir()
	clustertrusttest.Join(t, selfDir, "test-cluster", "self")
	clustertrusttest.Join(t, peerDir, "test-cluster", "peer")
	pin := func(dir, uuid, certDir string) {
		cert, err := os.ReadFile(filepath.Join(certDir, "node.crt"))
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(map[string]string{"nodeUuid": uuid, "certPem": string(cert)})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "trusted"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "trusted", uuid+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	pin(selfDir, "peer", peerDir)
	pin(peerDir, "self", selfDir)
	return selfDir, Open(selfDir), Open(peerDir)
}
func inventoryServer(t *testing.T, mesh *Mesh, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := mesh.VerifyClientPin(r); !ok {
			http.Error(w, "forbidden", 403)
			return
		}
		handler(w, r)
	}))
	srv.TLS = mesh.ServerTLSConfig()
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestReadPeerInventoryRequiresCurrentExactAuthorization(t *testing.T) {
	for _, change := range []string{"none", "pin", "admission"} {
		t.Run(change, func(t *testing.T) {
			dir, self, peer := inventoryPair(t)
			started, release := make(chan struct{}), make(chan struct{})
			srv := inventoryServer(t, peer, func(w http.ResponseWriter, _ *http.Request) {
				close(started)
				<-release
				_, _ = w.Write([]byte(`{"value":42}`))
			})
			pool := NewPeerClientPool(self, time.Second)
			defer pool.CloseIdle()
			client, ok := pool.Client("peer")
			if !ok {
				t.Fatal("missing peer client")
			}
			result := make(chan bool, 1)
			go func() {
				var value struct {
					Value int `json:"value"`
				}
				_, binding, ok := ReadPeerInventory(t.Context(), self, client, "peer", srv.URL, "background", 1024, &value)
				result <- ok && binding != "" && value.Value == 42
			}()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("request did not start")
			}
			if change == "pin" {
				clustertrusttest.RemovePeerPin(t, dir, "peer")
			}
			if change == "admission" {
				clustertrusttest.WriteAdmission(t, dir, "test-cluster", 2)
			}
			close(release)
			if accepted := <-result; accepted != (change == "none") {
				t.Fatalf("authorization change=%s, accepted=%v", change, accepted)
			}
		})
	}
}
func TestReadPeerInventoryRejectsRedirectsAndOversizedJSON(t *testing.T) {
	_, self, peer := inventoryPair(t)
	var publicReads atomic.Int32
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		publicReads.Add(1)
		_, _ = w.Write([]byte(`{"value":42}`))
	}))
	defer public.Close()
	srv := inventoryServer(t, peer, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, public.URL, http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(`{"value":42}` + strings.Repeat(" ", 1024)))
	})
	pool := NewPeerClientPool(self, time.Second)
	defer pool.CloseIdle()
	client, _ := pool.Client("peer")
	for _, path := range []string{"/redirect", "/oversized"} {
		var value struct{ Value int }
		if _, _, ok := ReadPeerInventory(t.Context(), self, client, "peer", srv.URL+path, "", 100, &value); ok {
			t.Fatalf("accepted %s", path)
		}
	}
	if publicReads.Load() != 0 {
		t.Fatal("authenticated read followed plaintext redirect")
	}
}
func TestPeerRecordIDRetainsAdmissionOnReadFailureButNotClear(t *testing.T) {
	dir := t.TempDir()
	clustertrusttest.Join(t, dir, "test-cluster", "self")
	mesh := Open(dir)
	before := mesh.PeerRecordID("self")
	if before == "" {
		t.Fatal("missing initial proof binding")
	}
	path := filepath.Join(dir, "admission.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	mesh.Refresh()
	if mesh.PeerRecordID("self") != before {
		t.Fatal("filesystem read failure discarded known admission")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	mesh.Refresh()
	if mesh.PeerRecordID("self") != "" {
		t.Fatal("absent admission retained authorization")
	}
}
