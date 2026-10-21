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

// affinityUsageChunk is the terminal chunk the fixture engine reports. It
// carries the phase values and the cached-token count oMLX puts in its usage
// block: the prefill and decode token counts, the three phase durations and
// the total time, plus the prompt tokens the engine served from its prefix
// cache. The numbers are the ones the broker's counterpart test copies.
const affinityUsageChunk = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":120," +
	"\"completion_tokens\":34,\"prompt_eval_duration\":1.25,\"generation_duration\":4.5," +
	"\"time_to_first_token\":1.3,\"total_time\":5.9," +
	"\"prompt_tokens_details\":{\"cached_tokens\":90}}}\n\n"

// waitAffinityWorkload returns the workloadInfo of the next workload:completed
// frame the proxy emitted.
func waitAffinityWorkload(t *testing.T, frames <-chan e2eFrame) json.RawMessage {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case frame := <-frames:
			if frame.Method != "workload:completed" {
				continue
			}
			var params struct {
				WorkloadInfo json.RawMessage `json:"workloadInfo"`
			}
			if err := json.Unmarshal(frame.Params, &params); err != nil {
				t.Fatal(err)
			}
			return params.WorkloadInfo
		case <-deadline:
			t.Fatal("timed out waiting for workload:completed")
			return nil
		}
	}
}

// assertAffinityMeasurement checks that the engine's measurement rides inline
// in the workloadInfo — the field names and the values the engine reported,
// not a nested object and not a proxy-measured substitute.
//
// The broker's counterpart test
// (services/nvpair-ui-broker/node_metrics_test.go) copies this frame's
// workloadInfo literal, so the two must agree by eye.
func assertAffinityMeasurement(t *testing.T, info json.RawMessage) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(info, &fields); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"prefillTokens": "120",
		"decodeTokens":  "34",
		"cachedTokens":  "90",
		"prefillMs":     "1250",
		"decodeMs":      "4500",
		"ttftMs":        "1300",
		"totalMs":       "5900",
	} {
		got, ok := fields[name]
		if !ok {
			t.Fatalf("the workload event omits %s: %s", name, info)
		}
		if string(got) != want {
			t.Fatalf("%s = %s, want %s", name, got, want)
		}
	}
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

// canonicalRequest re-marshals a JSON request body with its top-level keys
// sorted and its whitespace dropped, so two bodies can be compared without
// depending on the key order or the spacing either side used. The proxy
// re-marshals the body it forwards, so the comparison has to be
// order-insensitive.
func canonicalRequest(t *testing.T, body string) string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatalf("unmarshal request body %q: %v", body, err)
	}
	out, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal request body %q: %v", body, err)
	}
	return string(out)
}

// withoutUsageRequest removes the usage request the proxy adds to a
// streaming body, so the remainder can be compared with the client's own
// request. The proxy asks oMLX for the usage block, but it must not change
// anything else about the request it forwards.
func withoutUsageRequest(t *testing.T, body string) string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatalf("unmarshal engine body %q: %v", body, err)
	}
	delete(fields, "stream_options")
	out, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal engine body %q: %v", body, err)
	}
	return string(out)
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
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"%s-%s\"},\"finish_reason\":\"stop\"}]}\n\n", responseCanary, id)
			fmt.Fprint(w, affinityUsageChunk)
			fmt.Fprint(w, "data: [DONE]\n\n")
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
				if got.node != want {
					t.Fatalf("turn %d: expected node %s, got node %s", turn+1, want, got.node)
				}
				// The forwarded body is the client's request plus the usage
				// request the proxy adds; nothing else may differ.
				if stripped, want := withoutUsageRequest(t, got.body), canonicalRequest(t, string(body)); stripped != want {
					t.Fatalf("turn %d: expected the client's request, got %q", turn+1, stripped)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal([]byte(got.body), &fields); err != nil {
					t.Fatalf("unmarshal engine body %q: %v", got.body, err)
				}
				var options map[string]json.RawMessage
				if err := json.Unmarshal(fields["stream_options"], &options); err != nil {
					t.Fatalf("turn %d: engine body %q has no usage request: %v", turn+1, got.body, err)
				}
				if string(options["include_usage"]) != "true" {
					t.Fatalf("turn %d: engine body %q does not request the usage block", turn+1, got.body)
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
		if turn == 0 {
			// The terminal workload event carries the engine's own
			// measurement, inline in the workloadInfo. The broker folds
			// exactly this frame into its per-node totals.
			assertAffinityMeasurement(t, waitAffinityWorkload(t, frames))
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
