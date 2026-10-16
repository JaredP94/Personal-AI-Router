// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package ui

import (
	"context"
	"encoding/json"
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"net"
	svcerrors "nvpair-shared/errors"
	"nvpair-tui/rpc"
	"strings"
	"sync"
	"testing"
	"time"
)

type loop struct {
	m     Model
	inbox chan tea.Msg
}

func newLoop(m Model) *loop { return &loop{m, make(chan tea.Msg, 128)} }
func (l *loop) exec(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, cmd := range batch {
				l.exec(cmd)
			}
		} else if msg != nil {
			l.inbox <- msg
		}
	}()
}
func (l *loop) turn(t *testing.T, msg tea.Msg) {
	t.Helper()
	next, cmd := l.m.Update(msg)
	m, ok := next.(Model)
	if !ok {
		t.Fatalf("model %T", next)
	}
	l.m = m
	l.exec(cmd)
}
func (l *loop) until(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for !cond() {
		select {
		case msg := <-l.inbox:
			l.turn(t, msg)
		case <-deadline:
			t.Fatal("model did not converge")
		}
	}
}

type recoveryBroker struct {
	conn       net.Conn
	codec      *rpc.Codec
	writes     sync.Mutex
	mu         sync.Mutex
	epoch      string
	counts     map[string]int
	order      []string
	holdMethod string
	entered    chan struct{}
	release    chan struct{}
	fail       string
	broken     func(method string) bool // permanently failing getters
	workloads  []workload
}

func newRecoverySession(t *testing.T) (*recoveryBroker, *loop) {
	t.Helper()
	b, l := newRecoveryBroker(t)
	l.exec(l.m.setupConnection())
	l.until(t, func() bool { return l.m.connectionSetup })
	return b, l
}

// newRecoveryBroker wires a model to the fixture broker without running
// connection setup, so a test can shape setup's outcome first.
func newRecoveryBroker(t *testing.T) (*recoveryBroker, *loop) {
	t.Helper()
	cli, srv := net.Pipe()
	c := rpc.NewClient(cli, cli)
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	b := &recoveryBroker{conn: srv, codec: rpc.NewCodec(srv), epoch: "A", counts: map[string]int{}}
	t.Cleanup(func() {
		cancel()
		cli.Close()
		srv.Close()
		b.mu.Lock()
		if b.release != nil {
			close(b.release)
			b.release = nil
		}
		b.mu.Unlock()
	})
	go b.serve()
	return b, newLoop(New(c, nil, defaultViews(c)))
}
func (b *recoveryBroker) serve() {
	for {
		q, err := b.codec.Read()
		if err != nil {
			return
		}
		b.mu.Lock()
		b.counts[q.Method]++
		b.order = append(b.order, q.Method)
		epoch := b.epoch
		workloads := append([]workload{}, b.workloads...)
		gate := b.release
		if q.Method != b.holdMethod {
			gate = nil
		} else if b.entered != nil {
			close(b.entered)
			b.entered = nil
		}
		fail := q.Method == b.fail
		if fail {
			b.fail = ""
		}
		if b.broken != nil && b.broken(q.Method) {
			fail = true
		}
		b.mu.Unlock()
		if gate != nil {
			<-gate
		}
		var result any = map[string]any{}
		switch q.Method {
		case "cluster:get-node-id":
			result = map[string]string{"nodeUuid": epoch, "nodeId": epoch + "-host"}
		case "discovery:get-nodes", "nodes:get-initial", "nodes/list", "proxy:nodes/list", "lmstudio-proxy:nodes/list", "omlx-proxy:nodes/list":
			result = map[string]any{"nodes": []any{}}
		case "engine:get-installed":
			result = map[string]any{"engines": []any{}}
		case "errors:get-initial":
			result = []any{}
		case "workloads:get-initial":
			// The broker's real shape: workloadsInitialResult{Workloads}.
			result = map[string]any{"workloads": workloads}
		}
		b.writes.Lock()
		if fail {
			_ = b.codec.RespondError(q.ID, -32603, "fixture failure")
		} else {
			_ = b.codec.Respond(q.ID, result)
		}
		b.writes.Unlock()
	}
}
func (b *recoveryBroker) push(t *testing.T, epoch string, seq int, method string, params any) {
	t.Helper()
	b.mu.Lock()
	b.epoch = epoch
	b.mu.Unlock()
	raw, _ := json.Marshal(params)
	frame := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"method\":%q,\"params\":%s,\"epoch\":%q,\"seq\":%d}\n", method, raw, epoch, seq)
	b.writes.Lock()
	_, err := b.conn.Write([]byte(frame))
	b.writes.Unlock()
	if err != nil {
		t.Fatal(err)
	}
}
func (b *recoveryBroker) snapshotCounts() map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	counts := make(map[string]int, len(b.counts))
	for k, v := range b.counts {
		counts[k] = v
	}
	return counts
}

// settle feeds every pending message through the model until nothing new has
// arrived for a short quiet window.
func (l *loop) settle(t *testing.T) {
	t.Helper()
	for {
		select {
		case msg := <-l.inbox:
			l.turn(t, msg)
		case <-time.After(200 * time.Millisecond):
			return
		}
	}
}
func (b *recoveryBroker) count(method string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.counts[method]
}
func (b *recoveryBroker) hold(method string) <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.holdMethod = method
	b.entered = make(chan struct{})
	b.release = make(chan struct{})
	return b.entered
}
func (b *recoveryBroker) unhold() {
	b.mu.Lock()
	defer b.mu.Unlock()
	close(b.release)
	b.release = nil
	b.holdMethod = ""
}
func awaitEntry(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("getter not entered")
	}
}

func TestRecoveryReplacesEveryOwningViewAndReinstallsConnectionSubscriptions(t *testing.T) {
	for i := 0; i < 2; i++ {
		b, l := newRecoverySession(t)
		n := l.m.views[2].(*nodesView)
		w := l.m.views[4].(*workloadsView)
		cl := l.m.views[6].(*clusterView)
		h := l.m.views[0].(*healthView)
		e := l.m.views[1].(*errorsView)
		n.setNodes([]availableNode{{HostUUID: "stale"}})
		w.upsert(workload{ID: "stale", OriginatedFrom: "old"})
		cl.identity.NodeUUID = "old"
		h.localNodeUUID = "old"
		old := []svcerrors.ServiceError{{ID: crashPrefix + "scanner", NodeID: "old"}}
		h.rebuildCrashes(old)
		e.setErrors(old)
		b.push(t, "B", 1, "heartbeat", map[string]any{})
		l.until(t, func() bool { return l.m.hydratedEpoch == "B" })
		if len(n.nodes) != 0 || len(w.byKey) != 0 || cl.identity.NodeUUID != "B" || h.localNodeUUID != "B" || len(h.crashed) != 0 || len(e.errs) != 0 {
			t.Fatalf("stale consumers: nodes %v workloads %v identity %v/%v crash %v errors %v", n.nodes, w.byKey, cl.identity, h.localNodeUUID, h.crashed, e.errs)
		}
		b.mu.Lock()
		order := append([]string(nil), b.order...)
		counts := make(map[string]int)
		for k, v := range b.counts {
			counts[k] = v
		}
		b.mu.Unlock()
		if len(order) < 7 || order[6] != "broker:resync" {
			t.Fatalf("subscriptions not before resync/getters: %v", order)
		}
		for method, count := range counts {
			if count != 1 {
				t.Errorf("%s queried %d times", method, count)
			}
		}
		for _, method := range []string{"broker:resync", "discovery:get-nodes", "workloads:get-initial", "engine:get-installed", "nodes:get-initial", "cluster:get-node-id", "errors:get-initial", "nodes/list", "settings/get-force-ports", "proxy:get-status", "proxy:nodes/list", "proxy:node/selected"} {
			if counts[method] != 1 {
				t.Errorf("%s not fetched once: %v", method, counts)
			}
		}
	}
}
func TestRecoverySerializesLatestEpochAndRejectsStaleResults(t *testing.T) {
	b, l := newRecoverySession(t)
	entry := b.hold("settings/get-cluster-friendly-name")
	b.push(t, "A", 1, "heartbeat", nil)
	l.until(t, func() bool { return l.m.recoveryActive })
	awaitEntry(t, entry)
	b.push(t, "B", 2, "heartbeat", nil)
	l.until(t, func() bool { return l.m.epoch == "B" })
	b.push(t, "C", 3, "heartbeat", nil)
	l.until(t, func() bool { return l.m.epoch == "C" })
	if b.count("broker:resync") != 1 {
		t.Fatal("overlapping burst")
	}
	b.unhold()
	// First completion is A: it must not touch any view and must start C.
	for {
		msg := <-l.inbox
		if _, ok := msg.(recoveryResultMsg); ok {
			l.turn(t, msg)
			break
		}
		l.turn(t, msg)
	}
	if l.m.views[6].(*clusterView).identity.NodeUUID == "A" {
		t.Fatal("stale A result applied")
	}
	l.until(t, func() bool { return l.m.hydratedEpoch == "C" })
	if b.count("broker:resync") != 2 || l.m.views[6].(*clusterView).identity.NodeUUID != "C" {
		t.Fatal("latest follow-up missing")
	}
	// An old finalizer cannot retire or overwrite the completed generation.
	l.turn(t, recoveryResultMsg{generation: 1, epoch: "A", results: []tea.Msg{clusterIdentityMsg{id: clusterIdentity{NodeUUID: "A"}}}})
	if l.m.views[6].(*clusterView).identity.NodeUUID != "C" {
		t.Fatal("stale finalizer applied")
	}
}
func TestRecoveryFailureRetriesOnSameEpochTrafficThenStaysSilentAcrossSeqGaps(t *testing.T) {
	b, l := newRecoverySession(t)
	b.mu.Lock()
	b.fail = "broker:resync"
	b.mu.Unlock()
	b.push(t, "A", 1, "heartbeat", nil)
	l.until(t, func() bool { return l.m.recoveryActive })
	l.until(t, func() bool { return !l.m.recoveryActive })
	if l.m.hydratedEpoch != "" {
		t.Fatal("failed hydration marked successful")
	}
	b.push(t, "A", 100, "heartbeat", nil)
	l.until(t, func() bool { return l.m.hydratedEpoch == "A" })
	for _, seq := range []int{1000, 2, 1} {
		b.push(t, "A", seq, "heartbeat", nil)
		select {
		case msg := <-l.inbox:
			l.turn(t, msg)
		case <-time.After(time.Second):
			t.Fatal("notification not delivered")
		}
	}
	if b.count("broker:resync") != 2 {
		t.Fatalf("healthy same-epoch/gap traffic fetched: %d", b.count("broker:resync"))
	}
}
func TestRecoveryPreservesConcurrentSnapshotPushesAndWorkloadTombstones(t *testing.T) {
	b, l := newRecoverySession(t)
	b.mu.Lock()
	b.workloads = []workload{{ID: "removed", OriginatedFrom: "x"}}
	b.mu.Unlock()
	entry := b.hold("errors:get-initial")
	b.push(t, "A", 1, "heartbeat", nil)
	l.until(t, func() bool { return l.m.recoveryActive })
	awaitEntry(t, entry)
	b.push(t, "A", 2, "discovery:nodes-changed", []availableNode{{HostUUID: "live"}})
	b.push(t, "A", 3, "workloads:upsert", map[string]any{"workloadInfo": workload{ID: "removed", OriginatedFrom: "x"}})
	b.push(t, "A", 4, "workloads:remove", map[string]string{"workloadId": "removed", "originatedFrom": "x"})
	b.push(t, "A", 5, "workloads:upsert", map[string]any{"workloadInfo": workload{ID: "live", OriginatedFrom: "x"}})
	l.until(t, func() bool { return len(l.m.recoveryPushes) == 3 })
	b.unhold()
	l.until(t, func() bool { return l.m.hydratedEpoch == "A" })
	n := l.m.views[2].(*nodesView)
	w := l.m.views[4].(*workloadsView)
	if len(n.nodes) != 1 || n.nodes[0].HostUUID != "live" || len(w.byKey) != 1 || w.byKey[workloadKey("x", "live")].ID != "live" {
		t.Fatalf("newer pushes overwritten: %v %v", n.nodes, w.byKey)
	}
}

// An unavailable optional worker is a steady state, not a reason to re-burst:
// every domain whose getter succeeded gets its baseline, the epoch counts as
// hydrated, and healthy same-epoch traffic issues no further RPCs at all.
func TestRecoveryAppliesHealthyDomainsAndStaysSilentWhenOptionalGettersFail(t *testing.T) {
	b, l := newRecoverySession(t)
	b.mu.Lock()
	b.broken = func(method string) bool {
		return method == "omlx-proxy:nodes/list" || method == "nodes/list" || method == "engine:get-installed" || strings.HasPrefix(method, "settings/")
	}
	b.mu.Unlock()
	n := l.m.views[2].(*nodesView)
	w := l.m.views[4].(*workloadsView)
	cl := l.m.views[6].(*clusterView)
	h := l.m.views[0].(*healthView)
	e := l.m.views[1].(*errorsView)
	n.setNodes([]availableNode{{HostUUID: "stale"}})
	w.upsert(workload{ID: "stale", OriginatedFrom: "old"})
	cl.identity.NodeUUID = "old"
	h.localNodeUUID = "old"
	old := []svcerrors.ServiceError{{ID: crashPrefix + "scanner", NodeID: "old"}}
	h.rebuildCrashes(old)
	e.setErrors(old)

	b.push(t, "A", 1, "heartbeat", map[string]any{})
	l.until(t, func() bool { return l.m.recoveryActive })
	l.until(t, func() bool { return !l.m.recoveryActive })
	l.settle(t)
	if l.m.hydratedEpoch != "A" {
		t.Errorf("epoch not hydrated after a successful resync with optional getter failures: %q", l.m.hydratedEpoch)
	}
	if len(n.nodes) != 0 || len(w.byKey) != 0 || cl.identity.NodeUUID != "A" || h.localNodeUUID != "A" || len(h.crashed) != 0 || len(e.errs) != 0 {
		t.Errorf("healthy domains not applied: nodes %v workloads %v identity %v/%v crash %v errors %v", n.nodes, w.byKey, cl.identity, h.localNodeUUID, h.crashed, e.errs)
	}
	// A failed getter reaches its own view as that view's ordinary error result.
	if eng := l.m.views[5].(*enginesView); !strings.Contains(eng.status, "load engines failed") {
		t.Errorf("engine getter failure not delivered to its view: status %q", eng.status)
	}

	before := b.snapshotCounts()
	for seq := 2; seq <= 11; seq++ {
		b.push(t, "A", seq, "heartbeat", map[string]any{})
		l.settle(t)
	}
	after := b.snapshotCounts()
	if after["broker:resync"] != 1 {
		t.Errorf("healthy same-epoch traffic re-sent broker:resync: %d total", after["broker:resync"])
	}
	for method, count := range after {
		if count != before[method] {
			t.Errorf("%s issued %d more time(s) while healthy in the same epoch", method, count-before[method])
		}
	}
}

// A failed connection setup must not leave the model deaf: it keeps consuming
// notifications, retries setup on later traffic, and then hydrates normally.
func TestSetupFailureKeepsConsumingAndRetriesOnLaterTraffic(t *testing.T) {
	b, l := newRecoveryBroker(t)
	b.mu.Lock()
	b.fail = "discovery:subscribe"
	b.mu.Unlock()
	l.exec(l.m.setupConnection())
	l.until(t, func() bool { return l.m.setupErr != nil })
	if l.m.connectionSetup || l.m.disconnected {
		t.Fatalf("setup failure misreported: setup=%v disconnected=%v", l.m.connectionSetup, l.m.disconnected)
	}
	b.push(t, "A", 1, "heartbeat", map[string]any{})
	l.until(t, func() bool { return l.m.connectionSetup && l.m.hydratedEpoch == "A" })
	if got := b.count("discovery:subscribe"); got != 2 {
		t.Fatalf("discovery:subscribe sent %d times, want the failed attempt plus one retry", got)
	}
	if got := b.count("broker:resync"); got != 1 {
		t.Fatalf("broker:resync sent %d times, want 1", got)
	}
}

// Setup retries are bounded: a broker that keeps rejecting subscriptions is not
// asked again on every heartbeat, but its notifications are still consumed.
func TestSetupFailureRetriesAreBounded(t *testing.T) {
	b, l := newRecoveryBroker(t)
	b.mu.Lock()
	b.broken = func(method string) bool { return method == "discovery:subscribe" }
	b.mu.Unlock()
	l.exec(l.m.setupConnection())
	l.until(t, func() bool { return l.m.setupErr != nil })
	for seq := 1; seq <= 10; seq++ {
		b.push(t, "A", seq, "heartbeat", map[string]any{})
		l.settle(t)
	}
	if l.m.epoch != "A" {
		t.Fatalf("notifications not consumed after setup failure: epoch %q", l.m.epoch)
	}
	if got := b.count("discovery:subscribe"); got != maxSetupAttempts {
		t.Fatalf("discovery:subscribe sent %d times, want %d", got, maxSetupAttempts)
	}
	if got := b.count("broker:resync"); got != 0 {
		t.Fatalf("broker:resync sent %d times without subscriptions", got)
	}
}
