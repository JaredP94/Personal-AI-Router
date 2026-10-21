// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nodemetrics is the broker's per-node inference-metrics accumulator.
//
// It holds, per node that has served traffic, the lifetime totals of the
// prefill/decode measurements PAIR has observed for that node and a rolling
// window of the recent completions used to compute that node's current rates.
// The per-model breakdown is kept alongside, because "this node is slow" is
// usually "this node is slow for this model".
//
// Like the broker's workloadstore package, this file is pure state plus the
// aggregation rule: no I/O and no RPC live here, so the ingest path and the
// JSON-RPC surface can be wired up without the rule depending on either.
//
// Three rules shape the numbers:
//
//   - The token rates are weighted sums, not means of rates. Prefill tok/s is
//     sum(prefill tokens) / sum(prefill seconds) over the window, the
//     weighting oMLX itself uses (generation_tps = sum(output tokens) /
//     sum(generation seconds)). Averaging per-request rates would give a
//     10-token request the same weight as a 10,000-token one.
//
//   - The mean time-to-first-token is a mean, not a rate: sum(ttft ms) over
//     the requests that reported a TTFT. Averaging durations across requests
//     is meaningful; averaging rates is not.
//
//   - The window runs on the broker's own clock. A measurement carries no
//     timestamp of its own that the store may trust: the timestamps in a
//     relayed event belong to the node that ran the inference, and the repo
//     already defends against cross-node clock skew elsewhere. Two members of
//     the cluster can therefore hold slightly different sample sets at a
//     window boundary and report slightly different instantaneous rates,
//     while their lifetime totals agree exactly.
//
// The store is safe for concurrent use: the broker records from its
// workload-ingest path and snapshots from its RPC path.
package nodemetrics

import (
	"sort"
	"sync"
	"time"

	"nvpair-shared/inferencemetrics"
)

// Totals is the lifetime total of the measurements for a node, or for one
// model on that node. The values are cumulative since the broker started: they
// never shrink, and they are not derived from the rolling window, so a node
// that goes idle keeps its history while its rates fall back to zero.
type Totals struct {
	Requests      int   `json:"requests"`
	PrefillTokens int   `json:"prefillTokens"`
	DecodeTokens  int   `json:"decodeTokens"`
	CachedTokens  int   `json:"cachedTokens"`
	PrefillMs     int64 `json:"prefillMs"`
	DecodeMs      int64 `json:"decodeMs"`
	TtftMs        int64 `json:"ttftMs"`
	// TtftRequests counts the requests that reported a time-to-first-token.
	// It is the denominator of the mean TTFT and is kept apart from Requests
	// because a request that reported no TTFT must not dilute the mean of the
	// ones that did.
	TtftRequests int `json:"ttftRequests"`
	// TokenRequests counts the requests that reported a token count, that is
	// at least one of the prefill, decode or cached counts. It is the
	// denominator of a "tokens per request" figure, so a request that
	// reported no tokens at all must not count towards it.
	TokenRequests int `json:"tokenRequests"`
	// PrefillRequests counts the requests that reported a prefill duration.
	// It separates "the engine reported no prefill phase" from "the engine
	// reported a zero-length prefill", which a rate of zero cannot express
	// on its own. It is not the denominator of the prefill rate — that rate
	// divides the summed tokens by the summed duration — but it is the
	// denominator of a mean prefill duration.
	PrefillRequests int `json:"prefillRequests"`
	// DecodeRequests counts the requests that reported a decode duration.
	// It separates "the engine reported no decode phase" from "the engine
	// reported a zero-length decode". It is not the denominator of the
	// decode rate — that rate divides the summed tokens by the summed
	// duration — but it is the denominator of a mean decode duration.
	DecodeRequests int `json:"decodeRequests"`
}

// Rates are the node's current throughput, computed over the rolling window.
//
// A rate whose denominator is zero is zero: never NaN, never +Inf. A node with
// no traffic in the window has no rate to report, and a NaN would poison
// every consumer of the number.
type Rates struct {
	PrefillTokensPerSecond float64 `json:"prefillTokensPerSecond"`
	DecodeTokensPerSecond  float64 `json:"decodeTokensPerSecond"`
	MeanTtftMs             float64 `json:"meanTtftMs"`
}

// ModelTotals is one model's lifetime totals on one node.
type ModelTotals struct {
	Model  string `json:"model"`
	Totals Totals `json:"totals"`
}

// NodeSnapshot is one node's metrics: its lifetime totals, its current rates,
// and the same totals broken down per model.
type NodeSnapshot struct {
	NodeId string        `json:"nodeId"`
	Totals Totals        `json:"totals"`
	Rates  Rates         `json:"rates"`
	Models []ModelTotals `json:"models"`
}

// Store is the broker's per-node metrics accumulator.
//
// The state is per node id: the lifetime totals, a per-model map of the same
// totals, and the rolling window of recent completions.
//
// A Store must be created with NewStore. The zero value has no clock and no
// node map, so its first use panics on a nil map write or a nil clock call;
// NewStore is the only supported constructor.
type Store struct {
	mu sync.Mutex
	// window is the length of the rolling window. A non-positive window
	// disables it: every rate is zero and only the lifetime totals are kept.
	window time.Duration
	nodes  map[string]*nodeState
	// now reads the clock. It is unexported and defaults to time.Now; tests
	// in this package override it to move the window without sleeping.
	now func() time.Time
}

// nodeState is the accumulator for one node.
type nodeState struct {
	totals Totals
	models map[string]*Totals
	// samples holds the completions inside the rolling window, oldest first.
	samples []sample
}

// sample is one completed inference inside the rolling window.
type sample struct {
	// at is the instant the store stamped the completion, on the store's own
	// clock. The window is measured on this clock.
	at            time.Time
	prefillTokens int
	decodeTokens  int
	prefillMs     int64
	decodeMs      int64
	ttftMs        int64
	// hasTtft records that the engine reported a time-to-first-token, so
	// that a request without one does not count towards the mean's
	// denominator.
	hasTtft bool
}

// NewStore returns an empty store whose rolling window is the given
// duration.
func NewStore(window time.Duration) *Store {
	return &Store{
		window: window,
		nodes:  make(map[string]*nodeState),
		now:    time.Now,
	}
}

// Record folds one completed inference into the node's lifetime totals, into
// the model's totals, and into the rolling window.
//
// The now argument is the caller's instant of the completion — the moment the
// caller saw it arrive. The store measures its window on its own clock: the
// sample is stamped with the store's own reading, and the caller's instant is
// honoured only as a lower bound on that stamp. A caller ahead of the
// store's clock is on a clock the store does not share (a relayed event
// carries the origin's timestamps, and the repo already defends against
// cross-node clock skew elsewhere), and such a sample must not be reported as
// a rate over an interval that never happened here.
//
// The consequence for a caller that passes a remote event's timestamp is
// worth stating plainly: the timestamp is not what ages the sample. The
// sample ages against the store's clock; the caller's instant can only pull
// the stamp forward, never pull it back, so the window keeps advancing on the
// broker's own timeline.
func (s *Store) Record(nodeID, model string, m inferencemetrics.Measurement, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n, ok := s.nodes[nodeID]
	if !ok {
		n = &nodeState{models: make(map[string]*Totals)}
		s.nodes[nodeID] = n
	}
	accumulate(&n.totals, m)

	mt, ok := n.models[model]
	if !ok {
		mt = &Totals{}
		n.models[model] = mt
	}
	accumulate(mt, m)

	stamp := s.now()
	if now.After(stamp) {
		stamp = now
	}
	n.samples = append(n.samples, sample{
		at:            stamp,
		prefillTokens: valueOf(m.PrefillTokens),
		decodeTokens:  valueOf(m.DecodeTokens),
		prefillMs:     valueOf64(m.PrefillMs),
		decodeMs:      valueOf64(m.DecodeMs),
		ttftMs:        valueOf64(m.TtftMs),
		hasTtft:       m.TtftMs != nil,
	})
	s.pruneLocked(n, stamp)
}

// Snapshot returns one entry per node that has ever recorded traffic, ordered
// by node id, with its per-model entries ordered by model id.
//
// A node whose window is empty still appears, with zeroed rates and its
// lifetime totals intact: "idle now" is different from "never served". A node
// that never served anything does not appear at all.
func (s *Store) Snapshot() []NodeSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	end := s.now()
	out := make([]NodeSnapshot, 0, len(s.nodes))
	for _, id := range s.nodeIDsLocked() {
		n := s.nodes[id]
		out = append(out, NodeSnapshot{
			NodeId: id,
			Totals: n.totals,
			Rates:  s.ratesFor(n, end),
			Models: s.modelsFor(n),
		})
	}
	return out
}

// nodeIDsLocked returns the node ids in order. Caller holds s.mu.
func (s *Store) nodeIDsLocked() []string {
	ids := make([]string, 0, len(s.nodes))
	for id := range s.nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// modelsFor returns the node's per-model totals, ordered by model id.
// Caller holds s.mu.
func (s *Store) modelsFor(n *nodeState) []ModelTotals {
	names := make([]string, 0, len(n.models))
	for name := range n.models {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]ModelTotals, 0, len(names))
	for _, name := range names {
		out = append(out, ModelTotals{Model: name, Totals: *n.models[name]})
	}
	return out
}

// ratesFor computes the node's current rates from the samples inside the
// window that ends at end.
//
// The token rates are weighted sums over the window, the weighting oMLX uses
// itself; the mean TTFT is a mean over the requests that reported one.
func (s *Store) ratesFor(n *nodeState, end time.Time) Rates {
	var r Rates
	var prefillTokens, decodeTokens int
	var prefillMs, decodeMs, ttftMs int64
	var ttftRequests int

	for _, sm := range n.samples {
		if _, ok := s.inWindow(end, sm.at); !ok {
			continue
		}
		prefillTokens += sm.prefillTokens
		decodeTokens += sm.decodeTokens
		prefillMs += sm.prefillMs
		decodeMs += sm.decodeMs
		if sm.hasTtft {
			ttftMs += sm.ttftMs
			ttftRequests++
		}
	}

	r.PrefillTokensPerSecond = tokensPerSecond(prefillTokens, prefillMs)
	r.DecodeTokensPerSecond = tokensPerSecond(decodeTokens, decodeMs)
	if ttftRequests > 0 {
		r.MeanTtftMs = float64(ttftMs) / float64(ttftRequests)
	}
	return r
}

// inWindow reports whether a sample stamped at belongs to the window that
// ends at end.
//
// The window is the interval [end-window, end]. A sample stamped ahead of end
// can only happen when the store's clock stepped backwards (an NTP
// correction, a resumed VM) or when the caller is on another clock domain;
// it is kept while it is ahead by no more than the window, because it is then
// the newest traffic the store has, and dropped beyond that, because its
// position in the window is meaningless.
func (s *Store) inWindow(end time.Time, at time.Time) (time.Duration, bool) {
	if s.window <= 0 {
		return 0, false
	}
	age := end.Sub(at)
	if age < 0 {
		if -age > s.window {
			return 0, false
		}
		return 0, true
	}
	if age > s.window {
		return 0, false
	}
	return age, true
}

// pruneLocked drops the samples that have left the window, so the store's
// memory is bounded by the traffic inside the window rather than by the
// broker's uptime. The lifetime totals are untouched: they are not derived
// from the window. Caller holds s.mu.
func (s *Store) pruneLocked(n *nodeState, now time.Time) {
	kept := n.samples[:0]
	for _, sm := range n.samples {
		if _, ok := s.inWindow(now, sm.at); ok {
			kept = append(kept, sm)
		}
	}
	n.samples = kept
}

// accumulate folds one measurement into a totals accumulator.
//
// Every record is one request, even when the engine reported nothing: a
// request that failed before it reached an engine is still traffic the node
// saw. Only the values the engine actually reported are added, so an absent
// value never contributes a zero to a sum it did not measure — and a
// measured zero contributes a zero, which is the same number, but for a
// reason the consumer can rely on.
//
// The presence counters follow the same rule: a request counts towards a
// counter only when the engine reported that value, so a request that
// reported nothing is a request and nothing more. This is what lets a
// consumer tell "the engine reported no prefill phase" from "the engine
// reported a zero-length prefill": the two differ in the counter even though
// their rates are both zero.
func accumulate(t *Totals, m inferencemetrics.Measurement) {
	t.Requests++
	t.PrefillTokens += valueOf(m.PrefillTokens)
	t.DecodeTokens += valueOf(m.DecodeTokens)
	t.CachedTokens += valueOf(m.CachedTokens)
	t.PrefillMs += valueOf64(m.PrefillMs)
	t.DecodeMs += valueOf64(m.DecodeMs)
	if m.PrefillTokens != nil || m.DecodeTokens != nil || m.CachedTokens != nil {
		t.TokenRequests++
	}
	if m.PrefillMs != nil {
		t.PrefillRequests++
	}
	if m.DecodeMs != nil {
		t.DecodeRequests++
	}
	if m.TtftMs != nil {
		t.TtftMs += *m.TtftMs
		t.TtftRequests++
	}
}

// tokensPerSecond divides the tokens of a phase by the summed duration of
// that phase. A zero (or negative) duration yields zero: there is no rate to
// report, and dividing by it would produce +Inf or NaN.
func tokensPerSecond(tokens int, ms int64) float64 {
	if ms <= 0 {
		return 0
	}
	return float64(tokens) / (float64(ms) / 1000)
}

// valueOf reads a measured count, treating an unreported value as zero.
func valueOf(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

// valueOf64 reads a measured duration, treating an unreported value as zero.
func valueOf64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
