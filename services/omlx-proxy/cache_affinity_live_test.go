// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"nvpair-shared/schedulerwire"
)

// Opt in with PAIR_OMLX_LIVE_URL=http://127.0.0.1:1235 and
// PAIR_OMLX_LIVE_MODEL=<advertised model>. Uses the existing engine without
// restarting it. This single-node smoke test demonstrates conversation and
// affinity telemetry compatibility; it cannot prove cross-node routing or
// KV-cache hits, and the latency samples are not a comparative benchmark.
func TestLiveOMLXCacheAffinityConversation(t *testing.T) {
	engineURL, model := os.Getenv("PAIR_OMLX_LIVE_URL"), os.Getenv("PAIR_OMLX_LIVE_MODEL")
	if engineURL == "" || model == "" {
		t.Skip("set PAIR_OMLX_LIVE_URL and PAIR_OMLX_LIVE_MODEL to exercise an existing local oMLX engine")
	}
	u, err := url.Parse(engineURL)
	if err != nil || u.Scheme != "http" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		t.Fatal("PAIR_OMLX_LIVE_URL must be a local HTTP origin")
	}
	host := u.Hostname()
	if host != "localhost" && !net.ParseIP(host).IsLoopback() {
		t.Fatal("live test only permits a loopback engine")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		t.Fatal("PAIR_OMLX_LIVE_URL requires an explicit valid port")
	}
	key := readOMLXAPIKey()
	client := &http.Client{Timeout: 2 * time.Minute}
	modelsRequest, err := http.NewRequest(http.MethodGet, strings.TrimRight(engineURL, "/")+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		modelsRequest.Header.Set("Authorization", "Bearer "+key)
	}
	modelsResponse, err := client.Do(modelsRequest)
	if err != nil {
		t.Fatalf("local oMLX model inventory unavailable: %v", err)
	}
	var inventory struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(modelsResponse.Body, 1024*1024)).Decode(&inventory)
	_ = modelsResponse.Body.Close()
	if modelsResponse.StatusCode != 200 || decodeErr != nil {
		t.Fatalf("local oMLX inventory status=%d decode_error=%v", modelsResponse.StatusCode, decodeErr)
	}
	found := false
	for _, item := range inventory.Data {
		if item.ID == model {
			found = true
		}
	}
	if !found {
		t.Fatal("requested live model is absent from oMLX inventory")
	}
	t.Logf("live metadata: platform=%s/%s go=%s model=%s server=%s; single-node smoke, no KV-cache-hit or speedup claim", runtime.GOOS, runtime.GOARCH, runtime.Version(), model, modelsResponse.Header.Get("Server"))
	const canary = "live-affinity-private-canary"
	canaries := []string{canary}
	if key != "" {
		canaries = append(canaries, key)
	}
	stdin, frames, proxyURL := startAffinityBinary(t, canaries...)
	e2eSend(t, stdin, 1, "node/add-manual", map[string]interface{}{"id": "live-omlx", "host": host, "port": port, "addresses": []string{host}, "models": []string{model}})
	e2eWaitResult(t, frames, "1", 5*time.Second)
	messages := []map[string]string{{"role": "system", "content": "Reply briefly. Test marker: " + canary}, {"role": "user", "content": "Say hello in one word."}}
	for turn := 0; turn < 5; turn++ {
		// A real broker publishes fresh authoritative scheduler snapshots after
		// completions. Drive that boundary here to retire prior reservations.
		id := 10 + turn
		e2eSend(t, stdin, id, "node/set-priority", schedulerwire.Priority{Nodes: []string{"live-omlx"}, Ranks: []schedulerwire.NodeRank{{ID: "live-omlx"}}})
		e2eWaitResult(t, frames, strconv.Itoa(id), 5*time.Second)
		body, err := json.Marshal(map[string]interface{}{"model": model, "messages": messages, "stream": true, "max_tokens": 16, "temperature": 0, "chat_template_kwargs": map[string]bool{"enable_thinking": false}})
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequest(http.MethodPost, proxyURL+"/v1/chat/completions", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		if key != "" {
			request.Header.Set("Authorization", "Bearer "+key)
		}
		started := time.Now()
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("turn %d request failed: %v", turn+1, err)
		}
		if response.StatusCode != 200 {
			_ = response.Body.Close()
			t.Fatalf("turn %d engine status=%d", turn+1, response.StatusCode)
		}
		var reply strings.Builder
		var firstText time.Duration
		done := false
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 4096), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				done = true
				continue
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				_ = response.Body.Close()
				t.Fatalf("turn %d: invalid SSE JSON", turn+1)
			}
			for _, choice := range chunk.Choices {
				if choice.Delta.Content != "" {
					if firstText == 0 {
						firstText = time.Since(started)
					}
					reply.WriteString(choice.Delta.Content)
				}
			}
		}
		_ = response.Body.Close()
		if scanner.Err() != nil || !done || reply.Len() == 0 {
			t.Fatalf("turn %d: incomplete stream done=%v content_bytes=%d read_error=%v", turn+1, done, reply.Len(), scanner.Err())
		}
		event := waitAffinityCompletion(t, frames)
		if event.Status != 200 || event.NodeID != "live-omlx" || event.CacheAffinity != (turn > 0) {
			t.Fatalf("turn %d: completion status=%d node=%s affinity=%v", turn+1, event.Status, event.NodeID, event.CacheAffinity)
		}
		t.Logf("turn=%d cache_affinity=%v first_content_ms=%.1f proxy_header_ttfb_ms=%d total_ms=%d", turn+1, event.CacheAffinity, float64(firstText.Microseconds())/1000, event.TTFB, time.Since(started).Milliseconds())
		messages = append(messages, map[string]string{"role": "assistant", "content": reply.String()}, map[string]string{"role": "user", "content": "Give another one-word greeting."})
	}
	e2eSend(t, stdin, 99, "shutdown", nil)
	e2eWaitResult(t, frames, "99", 5*time.Second)
}
