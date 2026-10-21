// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// The broker's per-node inference-metrics surface: the accumulator that
// counts what the engines reported, the snapshot it serves, and the push
// that keeps a subscribed client current.
//
// The concern lives in its own file, like the broker's other per-concern
// files (telemetry, workloadmanager, nodeinfo, discovery): broker.go is the
// session and dispatch machinery, and the metrics rules are not part of it.
package main

import (
	"context"
	"encoding/json"
	"log"
	"log/slog"
	"time"

	"nvpair-shared/inferencemetrics"

	"nvpair-ui-broker/nodemetrics"
	"nvpair-ui-broker/workloadstore"
)

const (
	// nodeMetricsWindow is the length of the rolling window the per-node
	// metrics store keeps its recent completions in. Sixty seconds is long
	// enough to smooth a burst of traffic and short enough that an idle
	// node's rates fall back to zero promptly.
	nodeMetricsWindow = 60 * time.Second
	// nodeMetricsPushIntervalDefault is the coalescing period of the
	// metrics push: at most one snapshot push per interval, however many
	// completions arrived in between.
	nodeMetricsPushIntervalDefault = time.Second
)

// NodeMetricsResult is the response to "metrics:get-node-metrics" and the
// payload of the "metrics:node-metrics" push.
//
// It carries the FULL snapshot, never a delta: the push is a state
// synchronization, so a client that missed one recovers by re-reading
// everything rather than by accumulating differences it never saw.
type NodeMetricsResult struct {
	Nodes []nodemetrics.NodeSnapshot `json:"nodes"`
}

// nodeMetricsWorkload is the part of a workload event the per-node
// accumulator needs: the model (the per-model breakdown is keyed by it), the
// lifecycle state (only a terminal event is a served request) and the two node
// ids (the node that served the job, and the node the job came from).
//
// The engine's measurement is embedded, so it is read from the workloadInfo's
// inline fields — the same wire shape the proxy's terminal event carries.
type nodeMetricsWorkload struct {
	Model          string `json:"model"`
	State          string `json:"state"`
	OriginatedFrom string `json:"originatedFrom"`
	ScheduledOn    string `json:"scheduledOn"`
	inferencemetrics.Measurement
}

// recordNodeMetrics folds a terminal workload event into the per-node
// accumulator.
//
// It is called only for an accepted, authoritative merge (the workload store
// returned true) and never for an inferred event, so a re-asserted terminal
// event (the workload-manager's heartbeat) and a synthesized failure (the
// node-loss sweep) both leave the totals untouched.
//
// Only a terminal event is counted. The broker sees a non-terminal event for
// every job it watches, and a job that is merely running is not a served
// request yet — counting it would count every job twice and put a request in
// the totals that the engine never measured.
//
// The node credited is the one that served the job (scheduledOn). A workload
// that never picked an executor falls back to the node it came from
// (originatedFrom), which is the only attribution such a job has.
//
// The store measures its rolling window on its own clock, so the instant
// passed here is the broker's own arrival time: a relayed event carries the
// origin's timestamps, and comparing those against the local clock is how
// cross-node clock skew gets mistaken for real time.
func (b *Broker) recordNodeMetrics(info json.RawMessage) {
	// NewBroker always installs the store. A hand-built broker (a test that
	// exercises another path) may not have one, and it then has no metrics
	// to record — the same nil-tolerant shape the other optional broker
	// fields (telemetry, regCache) have.
	if b.nodeMetrics == nil {
		return
	}
	var wl nodeMetricsWorkload
	if err := json.Unmarshal(info, &wl); err != nil {
		return
	}
	if !workloadstore.IsTerminal(wl.State) {
		return
	}
	node := wl.ScheduledOn
	if node == "" {
		node = wl.OriginatedFrom
	}
	if node == "" {
		return
	}
	b.nodeMetrics.Record(node, wl.Model, wl.Measurement, time.Now())
	b.markNodeMetricsDirty()
}

// markNodeMetricsDirty records that the snapshot changed since the last push.
func (b *Broker) markNodeMetricsDirty() {
	b.metricsMu.Lock()
	b.metricsDirty = true
	b.metricsMu.Unlock()
}

// getNodeMetrics returns the current per-node metrics snapshot: the lifetime
// totals, the windowed rates and the per-model breakdown of every node that
// has served traffic.
//
// NewBroker always installs the store. A hand-built broker (a test that
// exercises another path) may not have one, and it then has no metrics to
// report — the same nil-tolerant shape the write path has, and an empty
// snapshot rather than a panic.
func (b *Broker) getNodeMetrics() NodeMetricsResult {
	if b.nodeMetrics == nil {
		return NodeMetricsResult{}
	}
	return NodeMetricsResult{Nodes: b.nodeMetrics.Snapshot()}
}

// flushNodeMetrics pushes the current snapshot to a subscribed client, if the
// snapshot changed since the last push.
//
// The dirty flag is what makes the push coalesced: a burst of completions
// marks it dirty many times, but only the first flush after the burst pushes
// — and it pushes the newest state, not a delta. A client that is not
// subscribed is skipped, and the snapshot stays dirty so the first flush
// after it subscribes still delivers the current state.
func (b *Broker) flushNodeMetrics() {
	b.metricsMu.Lock()
	if !b.metricsDirty || !b.metricsSubscribed {
		b.metricsMu.Unlock()
		return
	}
	b.metricsDirty = false
	b.metricsMu.Unlock()

	// A hand-built broker (a test that exercises another path) may have no
	// codec: there is no transport to push to, so there is nothing to do.
	if b.codec == nil {
		return
	}
	if err := b.codec.Notify("metrics:node-metrics", b.getNodeMetrics()); err != nil {
		slog.Warn("emit metrics:node-metrics failed", "err", err)
	}
}

// runNodeMetricsPush pushes the per-node metrics snapshot on a fixed cadence.
//
// The cadence is the coalescing: at most one push per interval, carrying the
// newest state, however many completions arrived in between. A push per
// completion would put the broker's write path under load for no benefit.
func (b *Broker) runNodeMetricsPush(ctx context.Context) {
	interval := b.nodeMetricsPushInterval
	if interval <= 0 {
		interval = nodeMetricsPushIntervalDefault
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.flushNodeMetrics()
		}
	}
}

// handleGetNodeMetrics answers "metrics:get-node-metrics".
//
// The full per-node snapshot: lifetime totals, windowed rates and the
// per-model breakdown for every node that has served traffic. This is
// also how a client hydrates — the push carries the same object.
func (b *Broker) handleGetNodeMetrics(msg *Message) {
	// A hand-built broker (a test that exercises another path) may have no
	// codec: there is no transport to answer on, so there is nothing to do.
	if b.codec == nil {
		return
	}
	if err := b.codec.Respond(msg.ID, b.getNodeMetrics()); err != nil {
		log.Printf("failed to respond to metrics:get-node-metrics: %v", err)
	}
}

// subscribeNodeMetrics answers "metrics:subscribe".
//
// Opt-in, like workloads:subscribe: the metrics:node-metrics push only
// goes to a subscribed client.
func (b *Broker) subscribeNodeMetrics(msg *Message) {
	if b.codec == nil {
		return
	}
	b.metricsMu.Lock()
	b.metricsSubscribed = true
	b.metricsMu.Unlock()
	if err := b.codec.Respond(msg.ID, SubscriptionResult{Subscribed: true}); err != nil {
		log.Printf("failed to respond to metrics:subscribe: %v", err)
	}
}

// unsubscribeNodeMetrics answers "metrics:unsubscribe".
func (b *Broker) unsubscribeNodeMetrics(msg *Message) {
	if b.codec == nil {
		return
	}
	b.metricsMu.Lock()
	b.metricsSubscribed = false
	b.metricsMu.Unlock()
	if err := b.codec.Respond(msg.ID, SubscriptionResult{Subscribed: false}); err != nil {
		log.Printf("failed to respond to metrics:unsubscribe: %v", err)
	}
}
