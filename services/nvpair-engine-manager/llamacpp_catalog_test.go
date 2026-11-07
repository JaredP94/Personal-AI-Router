// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Shape and names verified with read-only Hugging Face API metadata, 2026-11-07.
const llamaRepoFixture = `{"id":"bartowski/Llama-3.2-1B-Instruct-GGUF","sha":"0123456789012345678901234567890123456789","private":false,"gated":false,"pipeline_tag":"text-generation","tags":["gguf"],"siblings":[{"rfilename":"Llama-3.2-1B-Instruct-Q4_K_M.gguf","size":24,"lfs":{"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":24}}]}`

func fixtureLlamaRepo(t *testing.T) llamaHubRepo {
	t.Helper()
	var r llamaHubRepo
	if err := json.Unmarshal([]byte(llamaRepoFixture), &r); err != nil {
		t.Fatal(err)
	}
	return r
}
func TestLlamaCatalogValidation(t *testing.T) {
	r := fixtureLlamaRepo(t)
	if _, ok := r.entry(true); !ok {
		t.Fatal("valid fixture rejected")
	}
	for _, id := range []string{"../outside/repo", "a/..", "a/b/c", "a/" + strings.Repeat("x", 100), "a/b\n", "a/b\\c"} {
		r := fixtureLlamaRepo(t)
		r.ID = id
		if _, ok := r.entry(true); ok {
			t.Fatalf("accepted %q", id)
		}
	}
	for _, name := range []string{"../Q4_K_M.gguf", "sub/Q4_K_M.gguf", "x..Q4_K_M.gguf", strings.Repeat("x", 210) + "Q4_K_M.gguf", "x-Q4_K_M-00001-of-00002.gguf", "x-Q4_K_M\n.gguf"} {
		r := fixtureLlamaRepo(t)
		r.Siblings[0].Name = name
		if _, ok := r.entry(true); ok {
			t.Fatalf("accepted %q", name)
		}
	}
	for _, mutate := range []func(*llamaHubRepo){func(r *llamaHubRepo) { r.Gated = json.RawMessage(`"auto"`) }, func(r *llamaHubRepo) { r.Private = nil }, func(r *llamaHubRepo) { r.Pipeline = "feature-extraction" }, func(r *llamaHubRepo) { r.Siblings[0].LFS.Size = llamaMaxModelSize + 1 }, func(r *llamaHubRepo) { r.Siblings = append(r.Siblings, r.Siblings[0]) }} {
		r := fixtureLlamaRepo(t)
		mutate(&r)
		if _, ok := r.entry(true); ok {
			t.Fatal("hostile fixture accepted")
		}
	}
}
func TestLlamaCatalogBoundsCacheTTL(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("limit") != "50" {
			t.Error("missing API bound")
		}
		rows := make([]llamaHubRepo, 70)
		for i := range rows {
			rows[i] = fixtureLlamaRepo(t)
		}
		_ = json.NewEncoder(w).Encode(rows)
	}))
	defer srv.Close()
	h := newLlamaHub()
	h.endpoint = srv.URL
	now := time.Now()
	h.now = func() time.Time { return now }
	rows, err := h.catalog(context.Background(), "llama")
	if err != nil || len(rows) != 50 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	rows[0].ID = "mutated"
	rows, err = h.catalog(context.Background(), "llama")
	if err != nil || calls != 1 || rows[0].ID == "mutated" {
		t.Fatal("cache not isolated")
	}
	now = now.Add(llamaCatalogTTL)
	_, _ = h.catalog(context.Background(), "llama")
	if calls != 2 {
		t.Fatal("TTL ignored")
	}
	for i := 0; i < 25; i++ {
		_, _ = h.catalog(context.Background(), fmt.Sprintf("q%d", i))
	}
	if len(h.cache) > 20 {
		t.Fatalf("unbounded cache: %d", len(h.cache))
	}
	before := calls
	for _, q := range []string{strings.Repeat("x", 101), "x\n"} {
		if _, err := h.catalog(context.Background(), q); err == nil {
			t.Fatal("query accepted")
		}
	}
	if calls != before {
		t.Fatal("invalid query reached network")
	}
}
