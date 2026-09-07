// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nvpair-shared/schedulerwire"
)

type affinityOutput struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (o *affinityOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buffer.Write(p)
}

// Isolate the test listener and cluster identity from any running PAIR app.
// Capture both JSON-RPC notifications and diagnostic logs for privacy checks.
func startAffinityBinary(t *testing.T, canaries ...string) (io.Writer, <-chan e2eFrame, string) {
	t.Helper()
	port := e2eFreePort(t)
	cmd := exec.Command(proxyBin, "--port", strconv.Itoa(port), "--ignore-persisted-port", "--cluster-dir", t.TempDir())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	output := &affinityOutput{}
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	frames := make(chan e2eFrame, 1024)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		e2eReadFrames(io.TeeReader(stdout, output), frames)
	}()
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		<-readDone
		output.mu.Lock()
		defer output.mu.Unlock()
		for i, canary := range canaries {
			if strings.Contains(output.buffer.String(), canary) {
				t.Errorf("privacy canary %d leaked into process output", i)
			}
		}
	})
	if got := e2eWaitReadyPort(t, frames, 10*time.Second); got != port {
		t.Fatalf("ready port = %d, want %d", got, port)
	}
	return stdin, frames, fmt.Sprintf("http://127.0.0.1:%d", port)
}

func waitAffinityCompletion(t *testing.T, frames <-chan e2eFrame) RequestEvent {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case frame := <-frames:
			if frame.Method != "proxy/request" {
				continue
			}
			var event RequestEvent
			if err := json.Unmarshal(frame.Params, &event); err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(frame.Params, &fields); err != nil {
				t.Fatal(err)
			}
			if _, present := fields["cache_affinity"]; !present {
				t.Fatal("completion omitted cache_affinity")
			}
			return event
		case <-deadline:
			t.Fatal("timed out waiting for proxy/request")
		}
	}
}

// This exercises the compiled service's stdio and HTTP boundaries. Two fixture
// engines prove routing decisions, not actual engine KV-cache residency.
func TestE2ECacheAffinityConversationAndFailover(t *testing.T) {
	const model = "omlx-fixture-model"
	const systemCanary = "affinity-private-system-canary"
	const userCanary = "affinity-private-user-canary"
	const responseCanary = "affinity-private-response-canary"
	var failA atomic.Bool
	type attempt struct{ node, body string }
	attempts := make(chan attempt, 16)
	engine := func(id string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read fixture body: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			attempts <- attempt{id, string(body)}
			if id == "a" && failA.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"%s-%s\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", responseCanary, id)
		}))
	}
	a, b := engine("a"), engine("b")
	defer a.Close()
	defer b.Close()
	stdin, frames, proxyURL := startAffinityBinary(t, systemCanary, userCanary, responseCanary)
	for i, server := range []*httptest.Server{a, b} {
		host, port := e2eSplitHostPort(t, server.URL)
		e2eSend(t, stdin, i+1, "node/add-manual", map[string]interface{}{"id": []string{"a", "b"}[i], "host": host, "port": port, "addresses": []string{host}, "models": []string{model}})
		e2eWaitResult(t, frames, strconv.Itoa(i+1), 5*time.Second)
	}
	messages := []map[string]string{{"role": "system", "content": systemCanary}, {"role": "user", "content": userCanary}}
	client := &http.Client{Timeout: 10 * time.Second}
	for turn := 0; turn < 5; turn++ {
		order := []string{"b", "a"}
		if turn == 0 {
			order = []string{"a", "b"}
		}
		if turn == 2 {
			failA.Store(true)
		}
		id := 10 + turn
		e2eSend(t, stdin, id, "node/set-priority", schedulerwire.Priority{Nodes: order, Ranks: []schedulerwire.NodeRank{{ID: "a"}, {ID: "b"}}})
		e2eWaitResult(t, frames, strconv.Itoa(id), 5*time.Second)
		body, err := json.Marshal(map[string]interface{}{"model": model, "messages": messages, "stream": true, "temperature": 0.2, "max_tokens": 16})
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Post(proxyURL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		result, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		wantNode := "a"
		if turn >= 2 {
			wantNode = "b"
		}
		reply := responseCanary + "-" + wantNode
		if readErr != nil || resp.StatusCode != 200 || !bytes.Contains(result, []byte(reply)) || !bytes.Contains(result, []byte("data: [DONE]")) {
			t.Fatalf("turn %d: invalid streamed response, status=%d read_error=%v", turn+1, resp.StatusCode, readErr)
		}
		wantAttempts := []string{wantNode}
		if turn == 2 {
			wantAttempts = []string{"a", "b"}
		}
		for _, want := range wantAttempts {
			select {
			case got := <-attempts:
				if got.node != want || got.body != string(body) {
					t.Fatalf("turn %d: expected node %s and byte-identical request, got node %s; body_equal=%v", turn+1, want, got.node, got.body == string(body))
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("turn %d: missing attempt on %s", turn+1, want)
			}
		}
		event := waitAffinityCompletion(t, frames)
		wantAffinity := turn == 1 || turn >= 3
		if event.Status != 200 || event.NodeID != wantNode || event.CacheAffinity != wantAffinity {
			t.Fatalf("turn %d: completion status=%d node=%s affinity=%v; want 200/%s/%v", turn+1, event.Status, event.NodeID, event.CacheAffinity, wantNode, wantAffinity)
		}
		messages = append(messages, map[string]string{"role": "assistant", "content": reply}, map[string]string{"role": "user", "content": userCanary + strconv.Itoa(turn)})
	}
	select {
	case extra := <-attempts:
		t.Fatalf("unexpected additional attempt on node %s", extra.node)
	default:
	}
	e2eSend(t, stdin, 99, "shutdown", nil)
	e2eWaitResult(t, frames, "99", 5*time.Second)
}
