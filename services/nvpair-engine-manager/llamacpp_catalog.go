// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const llamaCatalogTTL = 6 * time.Hour
const llamaCatalogTimeout = 20 * time.Second
const llamaMaxModelSize int64 = 64 << 30

var llamaComponent = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,95}$`)
var llamaFilename = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,199}\.gguf$`)
var llamaSHA = regexp.MustCompile(`^[a-f0-9]{64}$`)

// LlamaCatalogEntry is one single-file Q4_K_M model. Metadata is resolved again
// immediately before transfer; a catalog row is never authority for a download.
type LlamaCatalogEntry struct {
	ID     string `json:"id"`
	File   string `json:"file"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}
type llamaHubRepo struct {
	ID       string          `json:"id"`
	SHA      string          `json:"sha"`
	Private  *bool           `json:"private"`
	Gated    json.RawMessage `json:"gated"`
	Pipeline string          `json:"pipeline_tag"`
	Tags     []string        `json:"tags"`
	Siblings []struct {
		Name string `json:"rfilename"`
		Size int64  `json:"size"`
		LFS  *struct {
			SHA256 string `json:"sha256"`
			OID    string `json:"oid"`
			Size   int64  `json:"size"`
		} `json:"lfs"`
	} `json:"siblings"`
}
type llamaCatalogCache struct {
	at   time.Time
	rows []LlamaCatalogEntry
}
type llamaHub struct {
	client   *http.Client
	endpoint string
	now      func() time.Time
	mu       sync.Mutex
	cache    map[string]llamaCatalogCache
}

func newLlamaHub() *llamaHub {
	tr := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: llamaDial, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 10 * time.Second, MaxResponseHeaderBytes: 64 << 10, IdleConnTimeout: 30 * time.Second}
	return &llamaHub{endpoint: "https://huggingface.co", now: time.Now, cache: make(map[string]llamaCatalogCache), client: &http.Client{Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || req.URL.User != nil || len(via) >= 5 {
			return fmt.Errorf("unsafe model redirect")
		}
		return nil
	}}}
}
func validLlamaComponent(s string) bool {
	return llamaComponent.MatchString(s) && !strings.Contains(s, "..")
}
func validLlamaRepo(s string) bool {
	p := strings.Split(s, "/")
	return len(p) == 2 && validLlamaComponent(p[0]) && validLlamaComponent(p[1])
}
func validLlamaFile(s string) bool {
	return llamaFilename.MatchString(s) && !strings.Contains(s, "..") && !llamaSplitName.MatchString(s)
}
func llamaRepoID(id string) (string, error) {
	repo, ok := strings.CutSuffix(id, ":Q4_K_M")
	if !ok || !validLlamaRepo(repo) {
		return "", fmt.Errorf("invalid llama.cpp model ID")
	}
	return repo, nil
}
func (r llamaHubRepo) entry(requireLFS bool) (LlamaCatalogEntry, bool) {
	if !validLlamaRepo(r.ID) || r.Private == nil || *r.Private || string(r.Gated) != "false" || r.Pipeline != "text-generation" || !slices.Contains(r.Tags, "gguf") {
		return LlamaCatalogEntry{}, false
	}
	var entries []LlamaCatalogEntry
	for _, f := range r.Siblings {
		if !validLlamaFile(f.Name) || !strings.HasSuffix(strings.ToUpper(f.Name), "Q4_K_M.GGUF") {
			continue
		}
		row := LlamaCatalogEntry{ID: r.ID + ":Q4_K_M", File: f.Name}
		if f.LFS != nil {
			row.Size = f.LFS.Size
			row.SHA256 = f.LFS.SHA256
			if row.SHA256 == "" {
				row.SHA256 = strings.TrimPrefix(f.LFS.OID, "sha256:")
			}
			if row.Size <= 0 || row.Size > llamaMaxModelSize || !llamaSHA.MatchString(row.SHA256) || f.Size != row.Size {
				continue
			}
		} else if requireLFS {
			continue
		}
		entries = append(entries, row)
	}
	if len(entries) != 1 {
		return LlamaCatalogEntry{}, false
	}
	return entries[0], true
}

// Preserve cancellation and deadline causes without exposing request URLs or
// remote response contents in user-facing errors or the engine error pipeline.
func llamaRequestError(message string, err error) error {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, cause) {
			return fmt.Errorf("%s: %w", message, cause)
		}
	}
	return errors.New(message)
}

func llamaReadError(ctx context.Context, message string, err error) error {
	// Some transports return a plain read error when cancellation tears down
	// the body. Only at that read boundary may the operation context supply
	// the missing cause; never use this for integrity or filesystem failures.
	if ctx.Err() != nil {
		p, tracked := ctx.Value(activePullKey{}).(*activePull)
		if !tracked || p.requested.Load() || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = ctx.Err()
		}
	}
	return llamaRequestError(message, err)
}

func (h *llamaHub) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.endpoint+path, nil)
	if err != nil {
		return llamaRequestError("invalid catalog request", err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return llamaRequestError("model metadata request failed", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("model metadata returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20+1))
	if err != nil {
		return llamaReadError(ctx, "model metadata could not be read", err)
	}
	if len(data) > 4<<20 {
		return fmt.Errorf("model metadata exceeds limit")
	}
	if json.Unmarshal(data, out) != nil {
		return fmt.Errorf("invalid model metadata")
	}
	return nil
}
func (h *llamaHub) catalog(ctx context.Context, query string) ([]LlamaCatalogEntry, error) {
	if !utf8.ValidString(query) || utf8.RuneCountInString(query) > 100 || strings.IndexFunc(query, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
		return nil, fmt.Errorf("query must be at most 100 characters without control characters")
	}
	key := strings.TrimSpace(query)
	h.mu.Lock()
	cached, ok := h.cache[key]
	h.mu.Unlock()
	if ok && h.now().Sub(cached.at) < llamaCatalogTTL {
		return slices.Clone(cached.rows), nil
	}
	ctx, cancel := context.WithTimeout(ctx, llamaCatalogTimeout)
	defer cancel()
	publishers := []string{"ggml-org", "bartowski", "unsloth"}
	if key != "" {
		publishers = []string{""}
	}
	rows := make([]LlamaCatalogEntry, 0)
	for _, publisher := range publishers {
		q := url.Values{"filter": {"gguf"}, "pipeline_tag": {"text-generation"}, "limit": {"50"}, "full": {"true"}}
		if key != "" {
			q.Set("search", key)
		} else {
			q.Set("author", publisher)
		}
		var repos []llamaHubRepo
		if err := h.get(ctx, "/api/models?"+q.Encode(), &repos); err != nil {
			return nil, err
		}
		for i, r := range repos {
			if i >= 50 {
				break
			}
			if publisher != "" && !strings.HasPrefix(r.ID, publisher+"/") {
				continue
			}
			if row, ok := r.entry(false); ok {
				rows = append(rows, row)
			}
		}
	}
	h.mu.Lock()
	queries := len(h.cache)
	if _, ok := h.cache[""]; ok {
		queries--
	}
	if _, exists := h.cache[key]; key != "" && !exists && queries >= 20 {
		var oldest string
		var at time.Time
		for k, v := range h.cache {
			if k != "" && (at.IsZero() || v.at.Before(at)) {
				oldest, at = k, v.at
			}
		}
		delete(h.cache, oldest)
	}
	h.cache[key] = llamaCatalogCache{at: h.now(), rows: slices.Clone(rows)}
	h.mu.Unlock()
	return rows, nil
}
func (h *llamaHub) resolve(ctx context.Context, id string) (LlamaCatalogEntry, string, error) {
	repo, err := llamaRepoID(id)
	if err != nil {
		return LlamaCatalogEntry{}, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, llamaCatalogTimeout)
	defer cancel()
	var r llamaHubRepo
	if err := h.get(ctx, "/api/models/"+repo+"?blobs=true", &r); err != nil {
		return LlamaCatalogEntry{}, "", err
	}
	row, ok := r.entry(true)
	if !ok || row.ID != id || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(r.SHA) {
		return LlamaCatalogEntry{}, "", fmt.Errorf("model is not a public single-file Q4_K_M generative GGUF with verified LFS metadata")
	}
	return row, h.endpoint + "/" + repo + "/resolve/" + r.SHA + "/" + url.PathEscape(row.File), nil
}
