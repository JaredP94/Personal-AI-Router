// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodemetrics

import (
	"sync"
	"testing"
	"time"

	"nvpair-shared/inferencemetrics"
)

func TestRecordAccumulatesTotalsAndRates(t *testing.T) {
	s := NewStore(60 * time.Second)
	now := time.Unix(1700000000, 0)
	s.Record("node-a", "model-x", inferencemetrics.Measurement{
		PrefillTokens: inferencemetrics.IntPtr(1000),
		DecodeTokens:  inferencemetrics.IntPtr(200),
		PrefillMs:     inferencemetrics.Int64Ptr(2000),
		DecodeMs:      inferencemetrics.Int64Ptr(4000),
		TtftMs:        inferencemetrics.Int64Ptr(500),
	}, now)
	snap := s.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("nodes: %d", len(snap))
	}
	n := snap[0]
	if n.NodeId != "node-a" {
		t.Fatalf("node id: %s", n.NodeId)
	}
	if n.Totals.Requests != 1 || n.Totals.PrefillTokens != 1000 || n.Totals.DecodeTokens != 200 {
		t.Fatalf("totals: %+v", n.Totals)
	}
	if n.Totals.TtftRequests != 1 {
		t.Fatalf("ttft requests: %+v", n.Totals)
	}
	if n.Rates.PrefillTokensPerSecond != 500 {
		t.Fatalf("prefill tps: %v", n.Rates.PrefillTokensPerSecond)
	}
	if n.Rates.DecodeTokensPerSecond != 50 {
		t.Fatalf("decode tps: %v", n.Rates.DecodeTokensPerSecond)
	}
	if n.Rates.MeanTtftMs != 500 {
		t.Fatalf("mean ttft: %v", n.Rates.MeanTtftMs)
	}
}

func TestSnapshotBreaksDownByModel(t *testing.T) {
	s := NewStore(60 * time.Second)
	now := time.Unix(1700000000, 0)
	s.Record("node-a", "model-x", inferencemetrics.Measurement{DecodeTokens: inferencemetrics.IntPtr(10)}, now)
	s.Record("node-a", "model-y", inferencemetrics.Measurement{DecodeTokens: inferencemetrics.IntPtr(20)}, now)
	models := s.Snapshot()[0].Models
	if len(models) != 2 {
		t.Fatalf("models: %+v", models)
	}
	if models[0].Model != "model-x" || models[0].Totals.DecodeTokens != 10 {
		t.Fatalf("models: %+v", models)
	}
	if models[1].Model != "model-y" || models[1].Totals.DecodeTokens != 20 {
		t.Fatalf("models: %+v", models)
	}
}

func TestWindowExpiryZeroesRatesButKeepsTotals(t *testing.T) {
	s := NewStore(60 * time.Second)
	t0 := time.Unix(1700000000, 0)
	s.Record("node-a", "m", inferencemetrics.Measurement{
		PrefillTokens: inferencemetrics.IntPtr(100), PrefillMs: inferencemetrics.Int64Ptr(1000),
	}, t0)
	inside := s.Snapshot()
	if inside[0].Rates.PrefillTokensPerSecond != 100 {
		t.Fatalf("rates: %+v", inside[0].Rates)
	}
	// Move the store's clock past the window: the sample leaves the window,
	// the rates go to zero, and the totals since the broker started stay.
	// Note that this exercises the negative-age tolerance, not the ordinary
	// ageing: the
	// moved-back clock puts the sample ahead of the window end, so it is
	// dropped because it lies too far in the future. The ordinary ageing
	// path is covered by TestAgedSampleLeavesTheWindow.
	s.now = func() time.Time { return t0.Add(61 * time.Second) }
	after := s.Snapshot()
	if after[0].Rates.PrefillTokensPerSecond != 0 {
		t.Fatalf("expired rates: %+v", after[0].Rates)
	}
	if after[0].Totals.PrefillTokens != 100 {
		t.Fatalf("totals must not shrink: %+v", after[0].Totals)
	}
}

func TestZeroDenominatorYieldsZeroRate(t *testing.T) {
	s := NewStore(60 * time.Second)
	s.Record("node-a", "m", inferencemetrics.Measurement{DecodeTokens: inferencemetrics.IntPtr(5)}, time.Unix(1700000000, 0))
	if got := s.Snapshot()[0].Rates.DecodeTokensPerSecond; got != 0 {
		t.Fatalf("rate: %v", got)
	}
}

// TestAgedSampleLeavesTheWindow pins the ordinary expiry: a sample recorded
// inside the window ages out of it once the store's clock has moved past the
// window, without the node disappearing and without its totals shrinking.
func TestAgedSampleLeavesTheWindow(t *testing.T) {
	s := NewStore(60 * time.Second)
	t0 := time.Unix(1700000000, 0)
	s.now = func() time.Time { return t0 }
	s.Record("node-a", "m", inferencemetrics.Measurement{
		PrefillTokens: inferencemetrics.IntPtr(100), PrefillMs: inferencemetrics.Int64Ptr(1000),
	}, t0)
	if got := s.Snapshot()[0].Rates.PrefillTokensPerSecond; got != 100 {
		t.Fatalf("rates inside the window: %v", got)
	}
	s.now = func() time.Time { return t0.Add(61 * time.Second) }
	snap := s.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("an idle node must still appear: %d", len(snap))
	}
	if snap[0].Rates.PrefillTokensPerSecond != 0 {
		t.Fatalf("aged rates: %+v", snap[0].Rates)
	}
	if snap[0].Totals.PrefillTokens != 100 || snap[0].Totals.Requests != 1 {
		t.Fatalf("totals must survive the window: %+v", snap[0].Totals)
	}
}

// TestNodeThatNeverServedIsAbsent is the other half of the idle case: a node
// with no traffic at all is unknown, not idle, and must not appear as a zero
// row.
func TestNodeThatNeverServedIsAbsent(t *testing.T) {
	s := NewStore(60 * time.Second)
	if snap := s.Snapshot(); len(snap) != 0 {
		t.Fatalf("nodes: %+v", snap)
	}
}

// TestEmptyMeasurementStillCountsAsARequest pins that a request which never
// reached an engine is still traffic the node saw: it counts as a request and
// contributes no tokens, so it dilutes no rate (there is no duration to
// weight it by) but it does appear in the request count.
func TestEmptyMeasurementStillCountsAsARequest(t *testing.T) {
	s := NewStore(60 * time.Second)
	t0 := time.Unix(1700000000, 0)
	s.now = func() time.Time { return t0 }
	s.Record("node-a", "m", inferencemetrics.Measurement{}, t0)
	snap := s.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("nodes: %d", len(snap))
	}
	if snap[0].Totals.Requests != 1 {
		t.Fatalf("requests: %+v", snap[0].Totals)
	}
	if snap[0].Rates.PrefillTokensPerSecond != 0 || snap[0].Rates.DecodeTokensPerSecond != 0 {
		t.Fatalf("rates: %+v", snap[0].Rates)
	}
	if snap[0].Rates.MeanTtftMs != 0 {
		t.Fatalf("mean ttft: %v", snap[0].Rates.MeanTtftMs)
	}
}

// TestRatesAreWeightedSums pins that the token rates are sum/sum over the
// window — oMLX's own weighting — and not the mean of the per-request rates.
// Two requests of 100 tokens each, one produced in 1 s and one in 9 s, are
// 20 tok/s together; averaging their rates would report 55.6.
func TestRatesAreWeightedSums(t *testing.T) {
	s := NewStore(60 * time.Second)
	t0 := time.Unix(1700000000, 0)
	s.now = func() time.Time { return t0 }
	s.Record("node-a", "m", inferencemetrics.Measurement{
		DecodeTokens: inferencemetrics.IntPtr(100), DecodeMs: inferencemetrics.Int64Ptr(1000),
	}, t0)
	s.Record("node-a", "m", inferencemetrics.Measurement{
		DecodeTokens: inferencemetrics.IntPtr(100), DecodeMs: inferencemetrics.Int64Ptr(9000),
	}, t0)
	got := s.Snapshot()[0].Rates.DecodeTokensPerSecond
	if got != 20 {
		t.Fatalf("decode tps: %v", got)
	}
}

// TestMeanTtftCountsOnlyRequestsThatReportedOne pins the mean's denominator:
// requests that reported no time-to-first-token do not dilute the mean of the
// ones that did.
func TestMeanTtftCountsOnlyRequestsThatReportedOne(t *testing.T) {
	s := NewStore(60 * time.Second)
	t0 := time.Unix(1700000000, 0)
	s.now = func() time.Time { return t0 }
	s.Record("node-a", "m", inferencemetrics.Measurement{TtftMs: inferencemetrics.Int64Ptr(100)}, t0)
	s.Record("node-a", "m", inferencemetrics.Measurement{DecodeTokens: inferencemetrics.IntPtr(1)}, t0)
	s.Record("node-a", "m", inferencemetrics.Measurement{TtftMs: inferencemetrics.Int64Ptr(300)}, t0)
	snap := s.Snapshot()[0]
	if snap.Totals.Requests != 3 {
		t.Fatalf("requests: %+v", snap.Totals)
	}
	if snap.Totals.TtftRequests != 2 {
		t.Fatalf("ttft requests: %+v", snap.Totals)
	}
	if snap.Rates.MeanTtftMs != 200 {
		t.Fatalf("mean ttft: %v", snap.Rates.MeanTtftMs)
	}
}

// TestPresenceCountersAreZeroWhenNothingWasReported pins that a request the
// engine reported nothing about is still a request, but counts towards none
// of the presence counters: the counters say what the engine reported, not
// what the node saw.
func TestPresenceCountersAreZeroWhenNothingWasReported(t *testing.T) {
	s := NewStore(60 * time.Second)
	t0 := time.Unix(1700000000, 0)
	s.now = func() time.Time { return t0 }
	s.Record("node-a", "m", inferencemetrics.Measurement{}, t0)
	totals := s.Snapshot()[0].Totals
	if totals.Requests != 1 {
		t.Fatalf("requests: %+v", totals)
	}
	if totals.TokenRequests != 0 || totals.PrefillRequests != 0 ||
		totals.DecodeRequests != 0 || totals.TtftRequests != 0 {
		t.Fatalf("presence counters must be zero: %+v", totals)
	}
}

// TestPresenceCounterCountsOnlyTheReportedValue pins that each presence
// counter tracks its own value: a measurement that reported tokens and a
// prefill duration increments the token and prefill counters, and leaves the
// decode and TTFT counters at zero even though it was a request.
func TestPresenceCounterCountsOnlyTheReportedValue(t *testing.T) {
	s := NewStore(60 * time.Second)
	t0 := time.Unix(1700000000, 0)
	s.now = func() time.Time { return t0 }
	s.Record("node-a", "m", inferencemetrics.Measurement{
		PrefillTokens: inferencemetrics.IntPtr(100),
		PrefillMs:     inferencemetrics.Int64Ptr(1000),
	}, t0)
	totals := s.Snapshot()[0].Totals
	if totals.Requests != 1 {
		t.Fatalf("requests: %+v", totals)
	}
	if totals.TokenRequests != 1 {
		t.Fatalf("token requests: %+v", totals)
	}
	if totals.PrefillRequests != 1 {
		t.Fatalf("prefill requests: %+v", totals)
	}
	if totals.DecodeRequests != 0 {
		t.Fatalf("decode requests: %+v", totals)
	}
	if totals.TtftRequests != 0 {
		t.Fatalf("ttft requests: %+v", totals)
	}
}

// TestPerModelTotalsSumToNodeTotals pins that the per-model breakdown is a
// partition of the node's totals: the per-model entries sum to the node's
// totals for every field, the presence counters included.
func TestPerModelTotalsSumToNodeTotals(t *testing.T) {
	s := NewStore(60 * time.Second)
	t0 := time.Unix(1700000000, 0)
	s.now = func() time.Time { return t0 }
	s.Record("node-a", "model-x", inferencemetrics.Measurement{
		PrefillTokens: inferencemetrics.IntPtr(100),
		DecodeTokens:  inferencemetrics.IntPtr(50),
		PrefillMs:     inferencemetrics.Int64Ptr(1000),
		DecodeMs:      inferencemetrics.Int64Ptr(2000),
		TtftMs:        inferencemetrics.Int64Ptr(300),
	}, t0)
	s.Record("node-a", "model-x", inferencemetrics.Measurement{
		DecodeTokens: inferencemetrics.IntPtr(25),
	}, t0)
	s.Record("node-a", "model-y", inferencemetrics.Measurement{
		CachedTokens: inferencemetrics.IntPtr(10),
		TtftMs:       inferencemetrics.Int64Ptr(100),
	}, t0)
	s.Record("node-a", "model-y", inferencemetrics.Measurement{}, t0)

	snap := s.Snapshot()[0]
	var sum Totals
	for _, m := range snap.Models {
		sum.Requests += m.Totals.Requests
		sum.PrefillTokens += m.Totals.PrefillTokens
		sum.DecodeTokens += m.Totals.DecodeTokens
		sum.CachedTokens += m.Totals.CachedTokens
		sum.PrefillMs += m.Totals.PrefillMs
		sum.DecodeMs += m.Totals.DecodeMs
		sum.TtftMs += m.Totals.TtftMs
		sum.TtftRequests += m.Totals.TtftRequests
		sum.TokenRequests += m.Totals.TokenRequests
		sum.PrefillRequests += m.Totals.PrefillRequests
		sum.DecodeRequests += m.Totals.DecodeRequests
	}
	if sum != snap.Totals {
		t.Fatalf("per-model sum %+v must equal node totals %+v", sum, snap.Totals)
	}
	// The counters must not be trivially equal because they are all zero.
	if snap.Totals.TokenRequests != 3 || snap.Totals.PrefillRequests != 1 ||
		snap.Totals.DecodeRequests != 1 || snap.Totals.TtftRequests != 2 {
		t.Fatalf("totals: %+v", snap.Totals)
	}
}

// TestSnapshotIsOrdered pins the deterministic ordering of the snapshot: nodes
// by id, models by id.
func TestSnapshotIsOrdered(t *testing.T) {
	s := NewStore(60 * time.Second)
	t0 := time.Unix(1700000000, 0)
	s.now = func() time.Time { return t0 }
	s.Record("node-b", "m2", inferencemetrics.Measurement{}, t0)
	s.Record("node-a", "m1", inferencemetrics.Measurement{}, t0)
	s.Record("node-a", "m0", inferencemetrics.Measurement{}, t0)
	snap := s.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("nodes: %+v", snap)
	}
	if snap[0].NodeId != "node-a" || snap[1].NodeId != "node-b" {
		t.Fatalf("node order: %+v", snap)
	}
	models := snap[0].Models
	if len(models) != 2 || models[0].Model != "m0" || models[1].Model != "m1" {
		t.Fatalf("model order: %+v", models)
	}
}

// TestConcurrentRecordAndSnapshot exercises the store the way the broker
// uses it: the ingest path records while the RPC path snapshots. Run under
// -race, it is the check that the store really is safe for concurrent use.
func TestConcurrentRecordAndSnapshot(t *testing.T) {
	s := NewStore(60 * time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				s.Record("node-a", "m", inferencemetrics.Measurement{
					PrefillTokens: inferencemetrics.IntPtr(10),
					PrefillMs:     inferencemetrics.Int64Ptr(100),
				}, time.Now())
			}
		}()
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = s.Snapshot()
			}
		}()
	}
	wg.Wait()
	snap := s.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("nodes: %+v", snap)
	}
	if snap[0].Totals.Requests != 200 {
		t.Fatalf("requests: %+v", snap[0].Totals)
	}
	if snap[0].Totals.PrefillTokens != 2000 {
		t.Fatalf("prefill tokens: %+v", snap[0].Totals)
	}
}
