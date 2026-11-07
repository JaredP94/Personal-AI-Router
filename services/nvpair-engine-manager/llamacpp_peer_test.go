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

func TestLlamaRemoteDeleteUsesIdentityAndRunningGuard(t *testing.T) {
	for _, mode := range []string{"delete", "running", "replacement"} {
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
			server := &controlServer{exec: ex}
			rec := httptest.NewRecorder()
			server.handleDelete(rec, httptest.NewRequest(http.MethodPost, controlDeletePath, strings.NewReader(`{"engine":"llamacpp","model":"`+llamaTestID+`"}`)))
			_, statErr := os.Stat(file)
			if mode == "delete" {
				if rec.Code != http.StatusOK || !os.IsNotExist(statErr) {
					t.Fatalf("delete: %d %s %v", rec.Code, rec.Body.String(), statErr)
				}
			} else if rec.Code == http.StatusOK || statErr != nil {
				t.Fatalf("unsafe delete: %d %s %v", rec.Code, rec.Body.String(), statErr)
			}
			if mode == "running" && !strings.Contains(rec.Body.String(), "stop the engine") {
				t.Fatalf("missing refusal reason: %s", rec.Body.String())
			}
		})
	}
}
