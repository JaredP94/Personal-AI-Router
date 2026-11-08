// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"

	"nvpair-shared/engineinventorytest"
	"nvpair-shared/noderec"
	"testing"
)

func TestPinnedScannerCacheErrorPreservesServedInventory(t *testing.T) {
	var enginePort int
	var manifest string
	var stop func()
	d, srv, _ := secureInventoryFixture(t, "uuid-peer", func(dir string) { enginePort, manifest, stop = engineinventorytest.Start(t, dir) })
	original := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		original.ServeHTTP(rec, r)
		if r.URL.Path != "/v1/node-info" || rec.Code != http.StatusOK {
			for key, values := range rec.Header() {
				w.Header()[key] = values
			}
			w.WriteHeader(rec.Code)
			_, _ = w.Write(rec.Body.Bytes())
			return
		}
		var info NodeInfoResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		info.PeerMetadata.Services[noderec.ServiceEngineManager] = enginePort
		_ = json.NewEncoder(w).Encode(info)
	})
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	raw := RawNode{ID: "cache-peer", Addresses: []string{host}, TXT: []string{"v=1", "uuid=uuid-peer", "cluster-uuid=uuid-peer", "ip=" + host, "ni=" + port}}
	d.onBrowse(DiscoveryEvent{Type: "discovered", Node: raw})
	before, ok := d.dir.get("uuid-peer")
	if !ok || !before.ServicePeer() || len(before.Models) != 2 {
		t.Fatalf("fixture: %+v", before)
	}
	if err := os.WriteFile(manifest, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	d.enrichModelsAt(&before, host)
	if !before.ServicePeer() || len(before.Models) != 2 || len(before.ModelsByEngine["ollama"]) != 1 || len(before.ModelsByEngine["lmstudio"]) != 1 {
		t.Fatalf("cache error damaged peer: %+v", before)
	}
	bytes, _ := json.Marshal(before)
	var wire struct {
		Errors map[string]string `json:"downloadErrorsByEngine"`
	}
	_ = json.Unmarshal(bytes, &wire)
	if wire.Errors["llamacpp"] != "cache_unavailable" || len(wire.Errors) != 1 {
		t.Fatalf("cache error lost: %s", bytes)
	}
	if !d.refreshNodeModelsCandidates(before.HostUUID, before.IP, []string{host}, enginePort, before.ClusterUUID) {
		t.Fatal("cache error alone did not update scanner snapshot")
	}
	published, _ := d.dir.get(before.HostUUID)
	if published.DownloadErrorsByEngine["llamacpp"] != "cache_unavailable" || len(published.Models) != 2 {
		t.Fatalf("periodic scanner refresh lost inventory or error: %+v", published)
	}
	if err := os.Remove(manifest); err != nil {
		t.Fatal(err)
	}
	d.enrichModelsAt(&before, host)
	bytes, _ = json.Marshal(before)
	wire.Errors = nil
	_ = json.Unmarshal(bytes, &wire)
	if len(wire.Errors) != 0 {
		t.Fatalf("recovered cache retained error: %s", bytes)
	}
	stop()
	d.enrichModelsAt(&before, host)
	if len(before.Models) != 2 || !before.ServicePeer() {
		t.Fatalf("whole-inventory failure changed scanner preservation: %+v", before)
	}
}
