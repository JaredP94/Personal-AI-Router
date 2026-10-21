// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"nvpair-shared/inferencemetrics"
	"nvpair-ui-broker/workloadstore"
)

// progressParams builds the params of a live progress frame.
func progressParams(t *testing.T, frame inferencemetrics.ProgressFrame) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// pairFactsFrame is the frame the origin proxy publishes: what PAIR measured
// of the client's stream.
func pairFactsFrame(jobID, nodeID string, elapsed int64) inferencemetrics.ProgressFrame {
	return inferencemetrics.ProgressFrame{
		JobID:     jobID,
		NodeID:    nodeID,
		ElapsedMs: inferencemetrics.Int64Ptr(elapsed),
		Chunks:    inferencemetrics.Int64Ptr(34),
		Bytes:     inferencemetrics.Int64Ptr(1820),
		TTFBMs:    inferencemetrics.Int64Ptr(260),
	}
}

// engineFrame is the frame the serving node's poller publishes: what the
// engine reported of its own request.
func engineFrame(jobID, nodeID string) inferencemetrics.ProgressFrame {
	return inferencemetrics.ProgressFrame{
		JobID:  jobID,
		NodeID: nodeID,
		Engine: &inferencemetrics.EngineProgress{
			RequestID:       "engine-req-1",
			Phase:           "generating",
			GeneratedTokens: inferencemetrics.IntPtr(34),
			TokensPerSecond: func() *float64 { v := 62.1; return &v }(),
			PrefillProgress: func() *float64 { v := 0.62; return &v }(),
			QueuePosition:   inferencemetrics.IntPtr(0),
		},
	}
}

// workloadUpsertParams builds a workloads:upsert notification for a job.
func workloadUpsertParams(t *testing.T, id, origin, scheduledOn, state string) json.RawMessage {
	t.Helper()
	info := map[string]any{
		"id":             id,
		"model":          "m",
		"engine":         "omlx",
		"runId":          "r1",
		"state":          state,
		"originatedFrom": origin,
		"scheduledOn":    scheduledOn,
		"createdAt":      1,
	}
	raw, err := json.Marshal(map[string]any{"workloadInfo": info})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// newLiveBroker builds a broker with an empty catalog, ready to ingest live
// frames.
func newLiveBroker() *Broker {
	return &Broker{workloads: workloadstore.New(), nodeID: "host"}
}

// TestProgressFrameUpdatesLiveView: a frame the local proxy published lands
// in the broker's live view of its job.
func TestProgressFrameUpdatesLiveView(t *testing.T) {
	b := newLiveBroker()

	b.routeProxyProgress(progressParams(t, pairFactsFrame("17", "node-b", 4200)))

	view, ok := b.liveViews().get("17", "node-b")
	if !ok {
		t.Fatal("the live view of job 17 is missing")
	}
	if view.ElapsedMs == nil || *view.ElapsedMs != 4200 {
		t.Fatalf("live elapsed = %v, want 4200", view.ElapsedMs)
	}
	if view.Chunks == nil || *view.Chunks != 34 {
		t.Fatalf("live chunks = %v, want 34", view.Chunks)
	}
	if view.Bytes == nil || *view.Bytes != 1820 {
		t.Fatalf("live bytes = %v, want 1820", view.Bytes)
	}
	if view.TTFBMs == nil || *view.TTFBMs != 260 {
		t.Fatalf("live ttfb = %v, want 260", view.TTFBMs)
	}
}

// TestProgressFrameFromPeerUpdatesTheSameView: the frame a peer relayed
// joins the same view as the frame this node's own proxy published. That is
// the whole point of the merge: for a peer-served job the two facts come
// from two different nodes.
func TestProgressFrameFromPeerUpdatesTheSameView(t *testing.T) {
	b := newLiveBroker()

	b.routeProxyProgress(progressParams(t, pairFactsFrame("17", "node-b", 4200)))
	b.forwardWorkloadManagerNotification(methodWorkloadsProgress, progressParams(t, engineFrame("17", "node-b")))

	view, ok := b.liveViews().get("17", "node-b")
	if !ok {
		t.Fatal("the live view of job 17 is missing")
	}
	if view.Engine == nil {
		t.Fatal("the relayed engine reading is missing from the live view")
	}
	if view.Engine.GeneratedTokens == nil || *view.Engine.GeneratedTokens != 34 {
		t.Fatalf("live generated tokens = %v, want 34", view.Engine.GeneratedTokens)
	}
	if view.ElapsedMs == nil || *view.ElapsedMs != 4200 {
		t.Fatalf("live elapsed = %v, want 4200 (the PAIR facts must survive the engine frame)", view.ElapsedMs)
	}
}

// TestProgressFrameMergeKeepsBothSides is the merge rule in both orders: a
// frame that carries only PAIR's facts must not erase the engine's reading
// another node reported, and a frame that carries only the engine's reading
// must not erase PAIR's facts.
func TestProgressFrameMergeKeepsBothSides(t *testing.T) {
	for _, order := range []string{"facts-then-engine", "engine-then-facts"} {
		t.Run(order, func(t *testing.T) {
			b := newLiveBroker()
			facts := pairFactsFrame("17", "node-b", 4200)
			engine := engineFrame("17", "node-b")
			if order == "facts-then-engine" {
				b.routeProxyProgress(progressParams(t, facts))
				b.forwardWorkloadManagerNotification(methodWorkloadsProgress, progressParams(t, engine))
			} else {
				b.forwardWorkloadManagerNotification(methodWorkloadsProgress, progressParams(t, engine))
				b.routeProxyProgress(progressParams(t, facts))
			}

			view, ok := b.liveViews().get("17", "node-b")
			if !ok {
				t.Fatal("the live view of job 17 is missing")
			}
			if view.Engine == nil || view.Engine.RequestID != "engine-req-1" {
				t.Fatalf("live engine reading = %+v, want the engine's own reading", view.Engine)
			}
			if view.ElapsedMs == nil || *view.ElapsedMs != 4200 {
				t.Fatalf("live elapsed = %v, want 4200 (PAIR's own facts must survive)", view.ElapsedMs)
			}
			if view.Chunks == nil || *view.Chunks != 34 {
				t.Fatalf("live chunks = %v, want 34 (PAIR's own facts must survive)", view.Chunks)
			}
		})
	}
}

// TestProgressFrameReachesTheLiveViewFromEveryProxy: all three inference
// proxies publish live frames, and every one of them must land in the live
// view. A routing that only the Ollama proxy's path recognised would leave
// the LM Studio and oMLX jobs without a live view.
func TestProgressFrameReachesTheLiveViewFromEveryProxy(t *testing.T) {
	cases := map[string]func(b *Broker, params json.RawMessage){
		"ollama": func(b *Broker, params json.RawMessage) {
			b.forwardProxyNotificationForGeneration(b.currentOllamaProxyGeneration(), methodProgress, params)
		},
		"lmstudio": func(b *Broker, params json.RawMessage) {
			b.forwardLMStudioProxyNotificationForGeneration(b.lmstudioProxyGeneration.Load(), methodProgress, params)
		},
		"omlx": func(b *Broker, params json.RawMessage) {
			b.forwardOMLXProxyNotificationForGeneration(b.omlxProxyGeneration.Load(), methodProgress, params)
		},
	}
	for name, forward := range cases {
		t.Run(name, func(t *testing.T) {
			b := newLiveBroker()
			forward(b, progressParams(t, pairFactsFrame("17", "node-b", 4200)))

			view, ok := b.liveViews().get("17", "node-b")
			if !ok {
				t.Fatal("the live view of job 17 is missing")
			}
			if view.ElapsedMs == nil || *view.ElapsedMs != 4200 {
				t.Fatalf("live elapsed = %v, want 4200", view.ElapsedMs)
			}
		})
	}
}

// TestProgressFrameDoesNotChangeTheJobState: a live frame is a snapshot of a
// running request, not a state transition. It must not create a catalog
// entry and must not touch the job's state.
func TestProgressFrameDoesNotChangeTheJobState(t *testing.T) {
	b := newLiveBroker()

	b.routeProxyProgress(progressParams(t, pairFactsFrame("17", "node-b", 4200)))

	if b.workloads.Len() != 0 {
		t.Fatalf("the catalog holds %d entries, want 0 (a frame is not a catalog event)", b.workloads.Len())
	}
}

// TestTerminalWorkloadEventRemovesLiveView: the job is over, so its live view
// goes away with it — and a frame that arrives late must not bring it back.
func TestTerminalWorkloadEventRemovesLiveView(t *testing.T) {
	b := newLiveBroker()

	b.routeProxyProgress(progressParams(t, pairFactsFrame("17", "node-b", 4200)))
	if _, ok := b.liveViews().get("17", "node-b"); !ok {
		t.Fatal("the live view of job 17 is missing")
	}

	b.emitWorkloadEvent("workloads:upsert", workloadUpsertParams(t, "17", "host", "node-b", "running"))
	b.emitWorkloadEvent("workloads:upsert", workloadUpsertParams(t, "17", "host", "node-b", "completed"))

	if _, ok := b.liveViews().get("17", "node-b"); ok {
		t.Fatal("the live view survived the job's terminal event")
	}

	// A frame that was already on its way must not resurrect the job.
	b.routeProxyProgress(progressParams(t, pairFactsFrame("17", "node-b", 4400)))
	if _, ok := b.liveViews().get("17", "node-b"); ok {
		t.Fatal("a late frame resurrected a retired job's live view")
	}
}

// TestLiveViewIsBounded: the live map is bounded, so a node that never sees
// a job's terminal event cannot accumulate views forever. The oldest view is
// the one that goes.
func TestLiveViewIsBounded(t *testing.T) {
	b := newLiveBroker()
	b.liveProgress = newLiveProgressIndex(4)

	for i := 0; i < 10; i++ {
		b.routeProxyProgress(progressParams(t, pairFactsFrame(fmt.Sprint(i), "node-b", 4200)))
	}

	if got := b.liveViews().len(); got != 4 {
		t.Fatalf("the live map holds %d views, want 4 (it must be bounded)", got)
	}
	if _, ok := b.liveViews().get("0", "node-b"); ok {
		t.Fatal("the oldest live view was kept, want it dropped")
	}
	if _, ok := b.liveViews().get("9", "node-b"); !ok {
		t.Fatal("the newest live view is missing")
	}
}

// TestProgressFrameIsPushedToSubscribedClients: the merged view reaches the
// clients that subscribed to the workload stream.
func TestProgressFrameIsPushedToSubscribedClients(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	b := newLiveBroker()
	b.codec = NewCodec(server)

	var mu sync.Mutex
	var pushed []*Message
	clientCodec := NewCodec(client)
	go func() {
		for {
			msg, err := clientCodec.Read()
			if err != nil {
				return
			}
			mu.Lock()
			pushed = append(pushed, msg)
			mu.Unlock()
		}
	}()

	b.workloadsMu.Lock()
	b.workloadsSubscribed = true
	b.workloadsMu.Unlock()

	b.routeProxyProgress(progressParams(t, pairFactsFrame("17", "node-b", 4200)))
	b.forwardWorkloadManagerNotification(methodWorkloadsProgress, progressParams(t, engineFrame("17", "node-b")))

	deadline := time.Now().Add(3 * time.Second)
	var got []*Message
	for time.Now().Before(deadline) {
		mu.Lock()
		got = append([]*Message(nil), pushed...)
		mu.Unlock()
		if len(got) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(got) < 2 {
		t.Fatalf("the client received %d push(es), want 2 (one per merged frame)", len(got))
	}
	for _, msg := range got {
		if msg.Method != methodWorkloadsProgress {
			t.Fatalf("pushed method = %q, want %q", msg.Method, methodWorkloadsProgress)
		}
		var view inferencemetrics.ProgressFrame
		if err := json.Unmarshal(msg.Params, &view); err != nil {
			t.Fatalf("pushed params are not a progress frame: %v", err)
		}
		if view.JobID != "17" {
			t.Fatalf("pushed job = %q, want 17", view.JobID)
		}
	}
}
