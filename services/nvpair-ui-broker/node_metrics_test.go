// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a concurrency-safe sink for the broker's outbound frames. The
// metrics push runs on its own goroutine, so a test may read the buffer while
// the codec is still writing to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// Reset drops everything written so far.
func (s *syncBuffer) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.Reset()
}

// Frames returns the outbound frames written so far, one per line.
func (s *syncBuffer) Frames() []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(s.String()), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// metricsSubscribedForTest reports the push gate.
func metricsSubscribedForTest(b *Broker) bool {
	b.metricsMu.Lock()
	defer b.metricsMu.Unlock()
	return b.metricsSubscribed
}

// setMetricsSubscribed flips the push gate.
func setMetricsSubscribed(b *Broker, subscribed bool) {
	b.metricsMu.Lock()
	b.metricsSubscribed = subscribed
	b.metricsMu.Unlock()
}

// nodeMetricsWorkload builds a terminal workload event for the metrics tests.
// The workload carries the engine's measurement inline, the way the proxy's
// terminal event does.
func nodeMetricsWorkloadEvent(id, state, scheduledOn string) json.RawMessage {
	return json.RawMessage(`{"workloadInfo":{"id":"` + id + `","model":"m","engine":"omlx","runId":"r",` +
		`"state":"` + state + `","originatedFrom":"node-a","scheduledOn":"` + scheduledOn + `",` +
		`"createdAt":1,"prefillTokens":100,"decodeTokens":20,"prefillMs":1000,"decodeMs":2000}}`)
}

// TestNodeMetricsAccumulateFromAcceptedWorkload covers the happy path: an
// accepted terminal workload is credited to the node that served it
// (scheduledOn), with the engine's measurement folded into that node's
// totals and into the per-model breakdown.
func TestNodeMetricsAccumulateFromAcceptedWorkload(t *testing.T) {
	b := NewBroker(NewCodec(writeOnlyReadWriter{io.Discard}), workerPaths{})
	b.applyWorkloadEvent("workloads:upsert", nodeMetricsWorkloadEvent("1", "completed", "node-b"), false, noSightingGuard)
	snap := b.getNodeMetrics()
	if len(snap.Nodes) != 1 {
		t.Fatalf("nodes: %+v", snap)
	}
	if snap.Nodes[0].NodeId != "node-b" {
		t.Fatalf("serving node: %s", snap.Nodes[0].NodeId)
	}
	if snap.Nodes[0].Totals.DecodeTokens != 20 {
		t.Fatalf("totals: %+v", snap.Nodes[0].Totals)
	}
	if snap.Nodes[0].Totals.PrefillTokens != 100 {
		t.Fatalf("totals: %+v", snap.Nodes[0].Totals)
	}
	if snap.Nodes[0].Totals.Requests != 1 {
		t.Fatalf("requests: %+v", snap.Nodes[0].Totals)
	}
	if snap.Nodes[0].Totals.DecodeMs != 2000 {
		t.Fatalf("totals: %+v", snap.Nodes[0].Totals)
	}
	if len(snap.Nodes[0].Models) != 1 {
		t.Fatalf("models: %+v", snap.Nodes[0].Models)
	}
	if snap.Nodes[0].Models[0].Model != "m" || snap.Nodes[0].Models[0].Totals.DecodeTokens != 20 {
		t.Fatalf("model totals: %+v", snap.Nodes[0].Models[0])
	}
}

// TestDuplicateWorkloadDoesNotDoubleCount covers the re-delivery case: the
// workload-manager re-asserts a terminal workload on its heartbeat, so the
// same event arrives twice. The second merge is a duplicate and must leave the
// totals untouched.
func TestDuplicateWorkloadDoesNotDoubleCount(t *testing.T) {
	b := NewBroker(NewCodec(writeOnlyReadWriter{io.Discard}), workerPaths{})
	event := nodeMetricsWorkloadEvent("1", "completed", "node-b")
	b.applyWorkloadEvent("workloads:upsert", event, false, noSightingGuard)
	b.applyWorkloadEvent("workloads:upsert", event, false, noSightingGuard)
	snap := b.getNodeMetrics()
	if len(snap.Nodes) != 1 {
		t.Fatalf("nodes: %+v", snap)
	}
	if snap.Nodes[0].Totals.Requests != 1 {
		t.Fatalf("requests: %+v", snap.Nodes[0].Totals)
	}
	if snap.Nodes[0].Totals.DecodeTokens != 20 {
		t.Fatalf("totals: %+v", snap.Nodes[0].Totals)
	}
}

// TestInferredWorkloadDoesNotCount covers the node-loss sweep: it synthesizes
// a terminal failure for a departed node's job, and that guess must never
// reach the accumulator.
func TestInferredWorkloadDoesNotCount(t *testing.T) {
	b := NewBroker(NewCodec(writeOnlyReadWriter{io.Discard}), workerPaths{})
	b.applyWorkloadEvent("workloads:upsert", nodeMetricsWorkloadEvent("1", "completed", "node-b"), true, noSightingGuard)
	snap := b.getNodeMetrics()
	if len(snap.Nodes) != 0 {
		t.Fatalf("an inferred event must not be counted: %+v", snap)
	}
}

// TestNodeMetricsAttributesToOriginWhenNotScheduled covers the fallback: a
// workload that never picked an executor has no scheduledOn, and the node it
// came from is the only attribution available.
func TestNodeMetricsAttributesToOriginWhenNotScheduled(t *testing.T) {
	b := NewBroker(NewCodec(writeOnlyReadWriter{io.Discard}), workerPaths{})
	b.applyWorkloadEvent("workloads:upsert", nodeMetricsWorkloadEvent("1", "completed", ""), false, noSightingGuard)
	snap := b.getNodeMetrics()
	if len(snap.Nodes) != 1 {
		t.Fatalf("nodes: %+v", snap)
	}
	if snap.Nodes[0].NodeId != "node-a" {
		t.Fatalf("origin fallback: %s", snap.Nodes[0].NodeId)
	}
}

// TestNodeMetricsCountsTerminalWorkloadsOnly covers the lifecycle: the broker
// sees a non-terminal event for every job it watches, so a job that is merely
// running must not be counted as a served request. The terminal event is what
// carries the measurement.
func TestNodeMetricsCountsTerminalWorkloadsOnly(t *testing.T) {
	b := NewBroker(NewCodec(writeOnlyReadWriter{io.Discard}), workerPaths{})
	b.applyWorkloadEvent("workloads:upsert", nodeMetricsWorkloadEvent("1", "running", "node-b"), false, noSightingGuard)
	if snap := b.getNodeMetrics(); len(snap.Nodes) != 0 {
		t.Fatalf("a running workload is not a served request: %+v", snap)
	}
	b.applyWorkloadEvent("workloads:upsert", nodeMetricsWorkloadEvent("1", "completed", "node-b"), false, noSightingGuard)
	snap := b.getNodeMetrics()
	if len(snap.Nodes) != 1 {
		t.Fatalf("nodes: %+v", snap)
	}
	if snap.Nodes[0].Totals.Requests != 1 {
		t.Fatalf("requests: %+v", snap.Nodes[0].Totals)
	}
	if snap.Nodes[0].Totals.DecodeTokens != 20 {
		t.Fatalf("totals: %+v", snap.Nodes[0].Totals)
	}
}

// TestNodeMetricsRPCSurface covers the JSON-RPC surface: the snapshot request
// and the subscription gate.
func TestNodeMetricsRPCSurface(t *testing.T) {
	var out syncBuffer
	b := NewBroker(NewCodec(writeOnlyReadWriter{&out}), workerPaths{})
	b.applyWorkloadEvent("workloads:upsert", nodeMetricsWorkloadEvent("1", "completed", "node-b"), false, noSightingGuard)

	id := json.RawMessage(`1`)
	b.handleMessage(&Message{JSONRPC: "2.0", ID: &id, Method: "metrics:get-node-metrics"})
	frames := out.Frames()
	if len(frames) != 1 {
		t.Fatalf("frames: %v", frames)
	}
	var resp struct {
		Result NodeMetricsResult `json:"result"`
		Error  *RPCError         `json:"error"`
	}
	if err := json.Unmarshal([]byte(frames[0]), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("error: %+v", resp.Error)
	}
	if len(resp.Result.Nodes) != 1 || resp.Result.Nodes[0].NodeId != "node-b" {
		t.Fatalf("result: %+v", resp.Result)
	}

	// The subscribe / unsubscribe handlers mirror workloads:subscribe and
	// answer the shared subscription result.
	out.Reset()
	id = json.RawMessage(`2`)
	b.handleMessage(&Message{JSONRPC: "2.0", ID: &id, Method: "metrics:subscribe"})
	var sub struct {
		Result SubscriptionResult `json:"result"`
	}
	if err := json.Unmarshal([]byte(out.Frames()[0]), &sub); err != nil {
		t.Fatal(err)
	}
	if !sub.Result.Subscribed {
		t.Fatalf("subscribe: %+v", sub)
	}
	if !metricsSubscribedForTest(b) {
		t.Fatal("subscribe must set the subscription flag")
	}

	out.Reset()
	id = json.RawMessage(`3`)
	b.handleMessage(&Message{JSONRPC: "2.0", ID: &id, Method: "metrics:unsubscribe"})
	if err := json.Unmarshal([]byte(out.Frames()[0]), &sub); err != nil {
		t.Fatal(err)
	}
	if sub.Result.Subscribed {
		t.Fatalf("unsubscribe: %+v", sub)
	}
	if metricsSubscribedForTest(b) {
		t.Fatal("unsubscribe must clear the subscription flag")
	}
}

// TestNodeMetricsPushIsGatedOnSubscription covers the push gate: the snapshot
// is only pushed to a subscribed client.
func TestNodeMetricsPushIsGatedOnSubscription(t *testing.T) {
	var out syncBuffer
	b := NewBroker(NewCodec(writeOnlyReadWriter{&out}), workerPaths{})
	b.applyWorkloadEvent("workloads:upsert", nodeMetricsWorkloadEvent("1", "completed", "node-b"), false, noSightingGuard)

	b.flushNodeMetrics()
	if len(out.Frames()) != 0 {
		t.Fatalf("an unsubscribed client must not be pushed to: %v", out.Frames())
	}

	setMetricsSubscribed(b, true)
	b.flushNodeMetrics()
	frames := out.Frames()
	if len(frames) != 1 {
		t.Fatalf("frames: %v", frames)
	}
	var pushed struct {
		Method string            `json:"method"`
		Params NodeMetricsResult `json:"params"`
	}
	if err := json.Unmarshal([]byte(frames[0]), &pushed); err != nil {
		t.Fatal(err)
	}
	if pushed.Method != "metrics:node-metrics" {
		t.Fatalf("method: %s", pushed.Method)
	}
	if len(pushed.Params.Nodes) != 1 || pushed.Params.Nodes[0].Totals.DecodeTokens != 20 {
		t.Fatalf("push payload: %+v", pushed.Params)
	}
}

// TestNodeMetricsPushIsCoalesced covers the push cadence: a burst of
// measurements is coalesced into a single push carrying the full snapshot,
// and the same state is not pushed again while it is unchanged.
func TestNodeMetricsPushIsCoalesced(t *testing.T) {
	var out syncBuffer
	b := NewBroker(NewCodec(writeOnlyReadWriter{&out}), workerPaths{})
	b.nodeMetricsPushInterval = 20 * time.Millisecond
	setMetricsSubscribed(b, true)

	for i := 0; i < 5; i++ {
		b.applyWorkloadEvent("workloads:upsert", nodeMetricsWorkloadEvent(string(rune('a'+i)), "completed", "node-b"), false, noSightingGuard)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.runNodeMetricsPush(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(out.Frames()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	frames := out.Frames()
	if len(frames) != 1 {
		t.Fatalf("a burst must be coalesced into one push, got %d: %v", len(frames), frames)
	}
	var pushed struct {
		Params NodeMetricsResult `json:"params"`
	}
	if err := json.Unmarshal([]byte(frames[0]), &pushed); err != nil {
		t.Fatal(err)
	}
	if len(pushed.Params.Nodes) != 1 {
		t.Fatalf("push payload: %+v", pushed.Params)
	}
	if pushed.Params.Nodes[0].Totals.Requests != 5 {
		t.Fatalf("the push carries the full snapshot: %+v", pushed.Params.Nodes[0].Totals)
	}
}
