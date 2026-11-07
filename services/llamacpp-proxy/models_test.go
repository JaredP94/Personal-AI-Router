// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLlamaModelListMappingAndExactIDs(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("engine path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"model"},{"id":"model:latest"}]}`))
	}))
	defer engine.Close()
	for _, path := range []string{"/v1/models", "/models"} {
		t.Run(path, func(t *testing.T) {
			disc := NewDiscovery()
			disc.AddManual(nodeFor(t, "local", engine.URL))
			p := testProxy(disc, defaultProxyPort)
			rec := httptest.NewRecorder()
			p.handleHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":"model"`) || !strings.Contains(rec.Body.String(), `"id":"model:latest"`) {
				t.Fatalf("model list: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
	if nodeAdvertisesModel(Node{Models: []string{"model:latest"}}, "model") {
		t.Fatal("model IDs must not acquire an implied tag")
	}
}
