// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMessagesRouteRole(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		inference    bool
	}{
		{http.MethodPost, "/v1/messages", true},
		{http.MethodGet, "/v1/messages", false},
		{http.MethodPut, "/v1/messages", false},
		{http.MethodPost, "/v1/messages/count_tokens", false},
	} {
		if got := isInferenceRequest(tc.method, tc.path); got != tc.inference {
			t.Errorf("%s %s inference = %v, want %v", tc.method, tc.path, got, tc.inference)
		}
	}
}

// Exercise real HTTP copying: the first SSE event must arrive while the engine
// is still waiting to finish, including after an advertised owner's 404.
func TestMessagesModelRoutingAndFailover(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[streaming], func(t *testing.T) {
			body := `{"model":"model","max_tokens":7,"messages":[{"role":"user","content":"test"}],"stream":` +
				map[bool]string{false: "false", true: "true"}[streaming] + `}`
			var missingHits, goodHits, wrongHits atomic.Int32
			wrong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wrongHits.Add(1)
			}))
			defer wrong.Close()
			check := func(r *http.Request) {
				t.Helper()
				data, err := io.ReadAll(r.Body)
				if err != nil || string(data) != body || r.URL.Path != "/v1/messages" {
					t.Error("engine did not receive the unchanged Messages request")
				}
				if r.Header.Get("x-api-key") != "synthetic-test-key" ||
					r.Header.Get("anthropic-version") != "2023-06-01" {
					t.Error("Anthropic request headers were not passed through")
				}
			}
			missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				missingHits.Add(1)
				check(r)
				w.WriteHeader(http.StatusNotFound)
			}))
			defer missing.Close()
			release := make(chan struct{})
			var once sync.Once
			finish := func() { once.Do(func() { close(release) }) }
			good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				goodHits.Add(1)
				check(r)
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: message_start\ndata: {}\n\n")
					w.(http.Flusher).Flush()
					<-release
					_, _ = io.WriteString(w, "event: message_stop\ndata: {}\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"type":"message","content":[]}`)
				}
			}))
			defer good.Close()
			defer finish()

			disc := NewDiscovery()
			disc.AddManual(nodeForModel(t, "wrong", wrong.URL, "different-model"))
			disc.AddManual(nodeForModel(t, "missing", missing.URL, "model"))
			disc.AddManual(nodeForModel(t, "good", good.URL, "model"))
			p := testProxy(disc, 0)
			defer p.closeIdleTransports()
			p.SetSelected("wrong")
			p.SetPriority([]string{"missing", "good"})
			server := httptest.NewServer(http.HandlerFunc(p.handleHTTP))
			defer func() {
				finish()
				server.Close()
			}()
			client := &http.Client{Timeout: 3 * time.Second}
			defer client.CloseIdleConnections()
			req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("x-api-key", "synthetic-test-key")
			req.Header.Set("anthropic-version", "2023-06-01")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if streaming {
				if resp.Header.Get("Content-Type") != "text/event-stream" {
					t.Fatal("SSE content type changed")
				}
				reader := bufio.NewReader(resp.Body)
				line, err := reader.ReadString('\n')
				if err != nil || line != "event: message_start\n" {
					t.Fatal("first SSE event did not arrive before engine completion")
				}
				finish()
				rest, err := io.ReadAll(reader)
				if err != nil || string(rest) != "data: {}\n\nevent: message_stop\ndata: {}\n\n" {
					t.Fatal("SSE response changed")
				}
			} else {
				data, err := io.ReadAll(resp.Body)
				if err != nil || string(data) != `{"type":"message","content":[]}` {
					t.Fatal("JSON response changed")
				}
			}
			if missingHits.Load() != 1 || goodHits.Load() != 1 || wrongHits.Load() != 0 {
				t.Fatal("request did not fail over only between advertised model owners")
			}
		})
	}
}

func TestMessagesFirstBodyStallCooldown(t *testing.T) {
	oldFirst, oldHeader := proxyFirstBodyTimeout, proxyResponseTimeout
	proxyFirstBodyTimeout, proxyResponseTimeout = 50*time.Millisecond, 3*time.Second
	defer func() { proxyFirstBodyTimeout, proxyResponseTimeout = oldFirst, oldHeader }()
	var stalledHits atomic.Int32
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stalledHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer stalled.Close()
	const event = "event: message_stop\ndata: {}\n\n"
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, event)
	}))
	defer good.Close()
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "stalled", stalled.URL, "model"))
	disc.AddManual(nodeForModel(t, "good", good.URL, "model"))
	p := testProxy(disc, 0)
	defer p.closeIdleTransports()
	for i := 0; i < 2; i++ {
		p.SetPriority([]string{"stalled", "good"})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		rec := httptest.NewRecorder()
		p.handleHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages",
			strings.NewReader(`{"model":"model","stream":true}`)).WithContext(ctx))
		cancel()
		if rec.Code != http.StatusOK || rec.Body.String() != event {
			t.Fatal("SSE first-body stall did not fail over")
		}
		if !p.coolingSet(time.Now())["stalled"] {
			t.Fatal("first-body stall did not cool candidate")
		}
	}
	if stalledHits.Load() != 1 {
		t.Fatal("second request retried cooling candidate before a ready peer")
	}
}

func TestMessagesCommittedSSENeverRetries(t *testing.T) {
	var goodHits atomic.Int32
	const partial = "event: message_start\ndata: {}\n\n"
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "999")
		_, _ = io.WriteString(w, partial)
		w.(http.Flusher).Flush()
	}))
	defer broken.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { goodHits.Add(1) }))
	defer good.Close()
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "broken", broken.URL, "model"))
	disc.AddManual(nodeForModel(t, "good", good.URL, "model"))
	p := testProxy(disc, 0)
	defer p.closeIdleTransports()
	p.SetPriority([]string{"broken", "good"})
	server := httptest.NewServer(http.HandlerFunc(p.handleHTTP))
	defer server.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Post(server.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"model","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err == nil || string(data) != partial || goodHits.Load() != 0 {
		t.Fatal("committed SSE was retried or not truncated")
	}
}

func TestMessagesOlderEngineReturnsError(t *testing.T) {
	var hits atomic.Int32
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"unsupported endpoint"}`)
	}))
	defer engine.Close()
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "old", engine.URL, "model"))
	p := testProxy(disc, 0)
	defer p.closeIdleTransports()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rec := httptest.NewRecorder()
	p.handleHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"model","stream":true}`)).WithContext(ctx))
	if rec.Code != http.StatusNotFound || rec.Body.String() != `{"error":"unsupported endpoint"}` || hits.Load() != 1 || ctx.Err() != nil {
		t.Fatal("older engine error was not returned cleanly after bounded candidates")
	}
}
