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
	proxyResponseTimeout = time.Second
	defer func() { proxyResponseTimeout = old }()

	for _, headers := range []bool{false, true} {
		name := "no-headers"
		if headers {
			name = "headers-without-body"
		}
		t.Run(name, func(t *testing.T) {
			var stalledHits, goodHits atomic.Int32
			release := make(chan struct{})
			stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				stalledHits.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				if headers {
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
				_, _ = io.Copy(io.Discard, r.Body)
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
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
					strings.NewReader(`{"model":"model"}`)).WithContext(ctx)
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
			t.Logf("first=%s second=%s (configured deadline %s)", first, second, proxyResponseTimeout)
			assertStallWorkloads(t, wire, 2)
		})
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
	old := proxyResponseTimeout
	proxyResponseTimeout = 100 * time.Millisecond
	defer func() { proxyResponseTimeout = old }()
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
	started := make(map[string]bool)
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
			started[wl.ID] = true
		case workloadCompletedMethod:
			if completed[wl.ID] || wl.ScheduledOn != "good" || !started[wl.ID] {
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
