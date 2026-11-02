// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nvpair-shared/schedulerwire"
)

type stallWire struct {
	mu sync.Mutex
	b  []byte
}

func (w *stallWire) Read([]byte) (int, error) { return 0, io.EOF }
func (w *stallWire) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.b = append(w.b, b...)
	return len(b), nil
}

// Removing deadline demotion must send the second request into the frozen
// upstream again. Reset scheduler reservations so they cannot hide that bug.
func TestStalledCandidateSecondRequest(t *testing.T) {
	old := proxyResponseTimeout
	// Leave scheduling slack between the measured stall and the next request.
	// First-body opt-ins below remain shorter and independent of this window.
	proxyResponseTimeout = 3 * time.Second
	defer func() { proxyResponseTimeout = old }()

	const originalBody = `{"model":"model","messages":[{"role":"user","content":"replay regression"}],"stream":false,"max_tokens":7}`
	for _, tc := range []struct {
		name, flag, env string
		headers         bool
	}{
		{name: "no-headers-default"},
		{name: "headers-without-body-flag", headers: true, flag: "1s"},
		{name: "headers-without-body-env", headers: true, env: "1s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(firstBodyTimeoutEnv, tc.env)
			oldFirstBody := proxyFirstBodyTimeout
			proxyFirstBodyTimeout = resolveFirstBodyTimeout(tc.flag)
			t.Cleanup(func() { proxyFirstBodyTimeout = oldFirstBody })
			var stalledHits, goodHits atomic.Int32
			release := make(chan struct{})
			stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				stalledHits.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				if tc.headers {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				<-release
			}))
			defer stalled.Close()
			defer close(release)
			good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				goodHits.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != originalBody {
					t.Error("healthy peer did not receive the complete original request body")
				}
				_, _ = io.WriteString(w, "healthy")
			}))
			defer good.Close()

			disc := NewDiscovery()
			disc.AddManual(nodeForModel(t, "stalled", stalled.URL, "model"))
			disc.AddManual(nodeForModel(t, "good", good.URL, "model"))
			wire := &stallWire{}
			p := NewProxy(NewCodec(wire), disc, 1235)
			defer p.closeIdleTransports()
			request := func() time.Duration {
				t.Helper()
				p.SetPrioritySnapshot(schedulerwire.Priority{
					Nodes: []string{"stalled", "good"},
					Ranks: []schedulerwire.NodeRank{{ID: "stalled"}, {ID: "good"}},
				})
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
					strings.NewReader(originalBody)).WithContext(ctx)
				rec := httptest.NewRecorder()
				start := time.Now()
				p.handleHTTP(rec, req)
				if rec.Code != http.StatusOK || rec.Body.String() != "healthy" {
					t.Fatalf("response = %d %q, want healthy failover", rec.Code, rec.Body.String())
				}
				return time.Since(start)
			}

			first := request()
			if stalledHits.Load() != 1 {
				t.Fatalf("first request frozen hits = %d, want 1", stalledHits.Load())
			}
			second := request()
			if stalledHits.Load() != 1 {
				t.Fatalf("second request paid the deadline again: frozen hits=%d; durations first=%s second=%s",
					stalledHits.Load(), first, second)
			}
			if goodHits.Load() != 2 {
				t.Fatalf("healthy hits = %d, want 2", goodHits.Load())
			}
			t.Logf("first=%s second=%s (header deadline %s, first-body deadline %s)", first, second, proxyResponseTimeout, proxyFirstBodyTimeout)
			assertStallWorkloads(t, wire, 2)
		})
	}
}

// A response-header deadline must not become a post-header generation limit.
// Long-context prefill can remain silent well beyond the header timeout.
func TestDefaultFirstBodyWaitIsUnbounded(t *testing.T) {
	t.Setenv(firstBodyTimeoutEnv, "")
	oldFirstBody := proxyFirstBodyTimeout
	proxyFirstBodyTimeout = resolveFirstBodyTimeout("")
	defer func() { proxyFirstBodyTimeout = oldFirstBody }()
	old := proxyResponseTimeout
	proxyResponseTimeout = 100 * time.Millisecond
	defer func() { proxyResponseTimeout = old }()
	var fallbackHits atomic.Int32
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(3 * proxyResponseTimeout)
		_, _ = io.WriteString(w, "prefill-complete")
	}))
	defer slow.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits.Add(1)
		_, _ = io.WriteString(w, "duplicate")
	}))
	defer fallback.Close()
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "slow", slow.URL, "model"))
	disc.AddManual(nodeForModel(t, "fallback", fallback.URL, "model"))
	p := testProxy(disc, 1235)
	defer p.closeIdleTransports()
	p.SetPriority([]string{"slow", "fallback"})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rec := httptest.NewRecorder()
	p.handleHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"model"}`)).WithContext(ctx))
	if rec.Code != http.StatusOK || rec.Body.String() != "prefill-complete" || fallbackHits.Load() != 0 {
		t.Fatalf("default cut off healthy post-header prefill: status=%d body=%q fallback hits=%d",
			rec.Code, rec.Body.String(), fallbackHits.Load())
	}
	if len(p.coolingSet(time.Now())) != 0 {
		t.Fatal("healthy prefill created a default cooldown")
	}
}

func TestCooldownOrderingRecovery(t *testing.T) {
	p := prProxy(t, "a", "b", "c", "d")
	p.SetPriority([]string{"a", "b", "c", "d"})
	p.SetSelected("a")
	now := time.Now()
	p.noteStalled("a", now)
	p.noteStalled("c", now)
	assertOrder(t, candidateIDs(p), []string{"b", "d", "a", "c"})
	p.noteSuccess("a")
	assertOrder(t, candidateIDs(p), []string{"a", "b", "d", "c"})
	if len(p.coolingSet(now.Add(proxyResponseTimeout))) != 0 {
		t.Fatal("cooldown did not expire without traffic or success")
	}
	assertOrder(t, candidateIDs(p), []string{"a", "b", "c", "d"})
	other := prProxy(t, "a", "b")
	p.noteStalled("a", now)
	assertOrder(t, candidateIDs(other), []string{"a", "b"})
}

func TestCooldownReservationAndAffinity(t *testing.T) {
	for _, tc := range []struct {
		name, warm, pin           string
		ids, ranked, cooled, want []string
	}{
		{name: "ranked", ids: []string{"a", "b"}, ranked: []string{"a", "b"}, cooled: []string{"a"}, want: []string{"b", "a"}},
		{name: "affinity", warm: "a", ids: []string{"a", "b"}, ranked: []string{"a", "b"}, cooled: []string{"a"}, want: []string{"b", "a"}},
		{name: "unlisted-peer", warm: "a", ids: []string{"a", "b"}, ranked: []string{"a"}, cooled: []string{"a"}, want: []string{"b", "a"}},
		{name: "manual-selection", pin: "a", ids: []string{"a", "b"}, ranked: []string{"a", "b"}, cooled: []string{"a"}, want: []string{"b", "a"}},
		{name: "all-cooling", ids: []string{"a", "b"}, ranked: []string{"a", "b"}, cooled: []string{"a", "b"}, want: []string{"a", "b"}},
		{name: "sole-cooling", ids: []string{"a"}, ranked: []string{"a"}, cooled: []string{"a"}, want: []string{"a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := prProxy(t, tc.ids...)
			p.SetPriority(tc.ranked)
			p.SetSelected(tc.pin)
			for _, id := range tc.cooled {
				p.noteStalled(id, time.Now())
			}
			got, affinity := p.reserveCandidateWithAffinity(reservationCandidates(tc.ids...), tc.warm)
			var ids []string
			for _, cand := range got {
				ids = append(ids, cand.id)
			}
			assertOrder(t, ids, tc.want)
			if affinity {
				t.Fatal("affinity promoted a cooling candidate over a ready peer")
			}
		})
	}
}

func TestPrecommitDropRetriesWithoutCooldown(t *testing.T) {
	dropped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A 200 followed by EOF before the declared body starts.
		w.Header().Set("Content-Length", "999")
		w.WriteHeader(http.StatusOK)
	}))
	defer dropped.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "healthy")
	}))
	defer good.Close()
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "dropped", dropped.URL, "model"))
	disc.AddManual(nodeForModel(t, "good", good.URL, "model"))
	wire := &stallWire{}
	p := NewProxy(NewCodec(wire), disc, 1235)
	defer p.closeIdleTransports()
	p.SetPriority([]string{"dropped", "good"})
	rec := httptest.NewRecorder()
	p.handleHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"model"}`)))
	if rec.Code != 200 || rec.Body.String() != "healthy" {
		t.Fatalf("precommit drop did not fail over: %d %q", rec.Code, rec.Body.String())
	}
	if len(p.coolingSet(time.Now())) != 0 {
		t.Fatal("fast body failure was incorrectly classified as a stall")
	}
	assertStallWorkloads(t, wire, 1)
}

func TestCommittedStreamNeverRetries(t *testing.T) {
	var goodHits atomic.Int32
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "999")
		_, _ = io.WriteString(w, "partial")
	}))
	defer broken.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodHits.Add(1)
		_, _ = io.WriteString(w, "healthy")
	}))
	defer good.Close()
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "broken", broken.URL, "model"))
	disc.AddManual(nodeForModel(t, "good", good.URL, "model"))
	p := testProxy(disc, 1235)
	defer p.closeIdleTransports()
	p.SetPriority([]string{"broken", "good"})
	server := httptest.NewServer(http.HandlerFunc(p.handleHTTP))
	defer server.Close()
	resp, err := server.Client().Post(server.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"model"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err == nil || string(data) != "partial" || goodHits.Load() != 0 {
		t.Fatalf("committed stream was retried or not truncated: body=%q error=%v goodHits=%d", data, err, goodHits.Load())
	}
}

func TestCooldownSuccessAndFastStatus(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusInternalServerError} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code) // empty 200 is a valid complete response
			}))
			defer engine.Close()
			disc := NewDiscovery()
			disc.AddManual(nodeForModel(t, "only", engine.URL, "model"))
			p := testProxy(disc, 1235)
			defer p.closeIdleTransports()
			p.noteStalled("only", time.Now())
			rec := httptest.NewRecorder()
			p.handleHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"model"}`)))
			if rec.Code != code {
				t.Fatalf("sole cooling candidate was not tried: status %d, want %d", rec.Code, code)
			}
			if cooling := p.coolingSet(time.Now())["only"]; cooling != (code != http.StatusOK) {
				t.Fatalf("cooling=%v for status %d: only success should clear", cooling, code)
			}
		})
	}
}

func TestCallerCancellationDoesNotCoolCandidate(t *testing.T) {
	for _, headers := range []bool{false, true} {
		t.Run(fmt.Sprint(headers), func(t *testing.T) {
			release := make(chan struct{})
			engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if headers {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				<-release
			}))
			defer engine.Close()
			defer close(release)
			disc := NewDiscovery()
			disc.AddManual(nodeForModel(t, "only", engine.URL, "model"))
			p := testProxy(disc, 1235)
			defer p.closeIdleTransports()
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			p.handleHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"model"}`)).WithContext(ctx))
			if len(p.coolingSet(time.Now())) != 0 {
				t.Fatal("a caller deadline demoted the candidate")
			}
		})
	}
}

func TestFinalBodyStallIsBounded(t *testing.T) {
	old := proxyFirstBodyTimeout
	proxyFirstBodyTimeout = 100 * time.Millisecond
	defer func() { proxyFirstBodyTimeout = old }()
	release := make(chan struct{})
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
	}))
	defer engine.Close()
	defer close(release)
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "only", engine.URL, "model"))
	p := testProxy(disc, 1235)
	defer p.closeIdleTransports()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rec := httptest.NewRecorder()
	p.handleHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"model"}`)).WithContext(ctx))
	if rec.Code != http.StatusBadGateway || ctx.Err() != nil {
		t.Fatalf("final body stall was not bounded before client deadline: status=%d context=%v", rec.Code, ctx.Err())
	}
	if !p.coolingSet(time.Now())["only"] {
		t.Fatal("final body stall was not remembered")
	}
}

func assertStallWorkloads(t *testing.T, wire *stallWire, count int) {
	t.Helper()
	wire.mu.Lock()
	data := append([]byte(nil), wire.b...)
	wire.mu.Unlock()
	started := make(map[string]string)
	completed := make(map[string]bool)
	dec := json.NewDecoder(strings.NewReader(string(data)))
	for dec.More() {
		var frame struct {
			Method string `json:"method"`
			Params struct {
				WorkloadInfo Workload `json:"workloadInfo"`
			} `json:"params"`
		}
		if err := dec.Decode(&frame); err != nil {
			t.Fatal(err)
		}
		wl := frame.Params.WorkloadInfo
		switch frame.Method {
		case workloadStartedMethod:
			if wl.State != "running" || completed[wl.ID] {
				t.Fatalf("invalid running workload event: %+v", wl)
			}
			started[wl.ID] = wl.ScheduledOn
		case workloadCompletedMethod:
			if started[wl.ID] != "good" {
				t.Fatalf("no running workload event on serving node before completion: id=%s runningOn=%q", wl.ID, started[wl.ID])
			}
			if completed[wl.ID] || wl.ScheduledOn != "good" {
				t.Fatalf("invalid or duplicate completion: %+v", wl)
			}
			completed[wl.ID] = true
		case workloadErroredMethod:
			t.Fatalf("failover emitted a failed workload: %+v", wl)
		}
	}
	if len(started) != count || len(completed) != count {
		t.Fatalf("workloads started=%v completed=%v, want %d identities", started, completed, count)
	}
}
