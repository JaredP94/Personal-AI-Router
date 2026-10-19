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
	clusterID  string
	counts     map[string]int
	order      []string
	holdMethod string
	entered    chan struct{}
	release    chan struct{}
	fail       string
	broken     func(method string) bool // permanently failing getters
	workloads  []workload
	workers    []workerStatus
	restarts   []map[string]string // params of every workers:restart received
}

// fixtureWorkers is a full broker lifecycle snapshot in the startup order the
// registry publishes, so recovery tests can assert all twelve rows appear.
func fixtureWorkers() []workerStatus {
	names := []string{
		"scanner", "errors", "settings", "engine-manager", "node-info", "proxy",
		"lmstudio-proxy", "omlx-proxy", "workload-manager", "manual-nodes",
		"cluster-manager", "scheduler",
	}
	workers := make([]workerStatus, 0, len(names))
	for _, name := range names {
		workers = append(workers, workerStatus{
			Name:          name,
			State:         "running",
			RestartOnHang: name != "engine-manager" && name != "proxy" && name != "lmstudio-proxy" && name != "omlx-proxy",
			PID:           1000,
		})
	}
	return workers
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
	b := &recoveryBroker{conn: srv, codec: rpc.NewCodec(srv), epoch: "A", counts: map[string]int{}, workers: fixtureWorkers()}
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
		epoch, clusterID := b.epoch, b.clusterID
		workloads := append([]workload{}, b.workloads...)
		workers := append([]workerStatus{}, b.workers...)
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
			result = map[string]string{"nodeUuid": epoch, "nodeId": epoch + "-host", "clusterId": clusterID}
		case "discovery:get-nodes", "nodes:get-initial", "nodes/list", "proxy:nodes/list", "lmstudio-proxy:nodes/list", "omlx-proxy:nodes/list":
			result = map[string]any{"nodes": []any{}}
		case "engine:get-installed":
			result = map[string]any{"engines": []any{}}
		case "errors:get-initial":
			result = []any{}
		case "workloads:get-initial":
			// The broker's real shape: workloadsInitialResult{Workloads}.
			result = map[string]any{"workloads": workloads}
		case "workers:get-status":
			result = map[string]any{"workers": workers}
		case "workers:restart":
			var params map[string]string
			_ = json.Unmarshal(q.Params, &params)
			b.mu.Lock()
			b.restarts = append(b.restarts, params)
			b.mu.Unlock()
			result = map[string]any{"name": params["name"], "state": "restarting"}
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

// restartParams returns the params of every workers:restart the broker saw.
func (b *recoveryBroker) restartParams() []map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]map[string]string(nil), b.restarts...)
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
		old := []svcerrors.ServiceError{{ID: "worker-error", NodeID: "old"}}
		h.setWorkers([]workerStatus{{Name: "stale-worker", State: "exhausted"}})
		e.setErrors(old)
		b.push(t, "B", 1, "heartbeat", map[string]any{})
		l.until(t, func() bool { return l.m.hydratedEpoch == "B" })
		if len(n.nodes) != 0 || len(w.byKey) != 0 || cl.identity.NodeUUID != "B" || len(h.workers) != 12 || len(e.errs) != 0 {
			t.Fatalf("stale consumers: nodes %v workloads %v identity %v workers %v errors %v", n.nodes, w.byKey, cl.identity, h.workers, e.errs)
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
		for _, method := range []string{"broker:resync", "discovery:get-nodes", "workloads:get-initial", "engine:get-installed", "nodes:get-initial", "cluster:get-node-id", "errors:get-initial", "workers:get-status", "nodes/list", "settings/get-force-ports", "proxy:get-status", "proxy:nodes/list", "proxy:node/selected"} {
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
	h.setWorkers([]workerStatus{{Name: "stale-worker", State: "exhausted"}})
	old := []svcerrors.ServiceError{{ID: "worker-error", NodeID: "old"}}
	e.setErrors(old)

	b.push(t, "A", 1, "heartbeat", map[string]any{})
	l.until(t, func() bool { return l.m.recoveryActive })
	l.until(t, func() bool { return !l.m.recoveryActive })
	l.settle(t)
	if l.m.hydratedEpoch != "A" {
		t.Errorf("epoch not hydrated after a successful resync with optional getter failures: %q", l.m.hydratedEpoch)
	}
	if len(n.nodes) != 0 || len(w.byKey) != 0 || cl.identity.NodeUUID != "A" || len(h.workers) != 12 || len(e.errs) != 0 {
		t.Errorf("healthy domains not applied: nodes %v workloads %v identity %v workers %v errors %v", n.nodes, w.byKey, cl.identity, h.workers, e.errs)
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

// newHydratedSession drives the fixture through one epoch recovery so the
// Overview holds the broker's full worker snapshot.
func newHydratedSession(t *testing.T) (*recoveryBroker, *loop) {
	t.Helper()
	b, l := newRecoverySession(t)
	b.push(t, "A", 1, "heartbeat", map[string]any{})
	l.until(t, func() bool { return l.m.hydratedEpoch == "A" })
	l.settle(t)
	return b, l
}

// The Overview is hydrated by the root epoch recovery path, not by a view's
// Init: the shell never calls Init for this tab, so a getter that lives there
// would leave the real program blank.
func TestOverviewHydratesAllTwelveWorkersFromEpochRecovery(t *testing.T) {
	b, l := newHydratedSession(t)
	if got := b.count("workers:get-status"); got != 1 {
		t.Fatalf("workers:get-status sent %d times, want exactly one recovery read", got)
	}
	h := l.m.views[0].(*healthView)
	if len(h.workers) != 12 {
		t.Fatalf("hydrated %d workers, want all 12 the broker supervises: %v", len(h.workers), h.workers)
	}
	rows := h.table.Rows()
	if len(rows) != 12 {
		t.Fatalf("table has %d rows, want 12", len(rows))
	}
	want := []string{"scanner", "errors", "settings", "engine-manager", "node-info", "proxy", "lmstudio-proxy", "omlx-proxy", "workload-manager", "manual-nodes", "cluster-manager", "scheduler"}
	for i, name := range want {
		if rows[i][0] != name {
			t.Errorf("row %d is %q, want %q in the broker's startup order", i, rows[i][0], name)
		}
	}
}

// workers:update is relayed unconditionally, so a change replaces the rows
// without another getter.
func TestOverviewAppliesWorkersUpdatePush(t *testing.T) {
	b, l := newHydratedSession(t)
	b.push(t, "A", 2, "workers:update", map[string]any{"workers": []map[string]any{
		{"name": "scanner", "state": "running", "attempts": 1, "pid": 77},
		{"name": "proxy", "state": "degraded", "attempts": 3, "restartOnHang": false, "lastError": "subprocess proxy stopped responding"},
	}})
	l.until(t, func() bool { return len(l.m.views[0].(*healthView).workers) == 2 })
	h := l.m.views[0].(*healthView)
	rows := h.table.Rows()
	if len(rows) != 2 || rows[1][0] != "proxy" || rows[1][1] != "degraded" {
		t.Fatalf("push did not replace the rows: %v", rows)
	}
	if got := b.count("workers:get-status"); got != 1 {
		t.Errorf("a workers:update push triggered %d extra getters", got-1)
	}
}

// The confirmation is the gate: n issues nothing at all.
func TestOverviewRestartCancelledSendsNoRPC(t *testing.T) {
	b, l := newHydratedSession(t)
	l.turn(t, keyRunes('R'))
	if !l.m.views[0].(*healthView).confirm.open() {
		t.Fatal("R on the selected worker did not open a confirmation")
	}
	if got := b.count("workers:restart"); got != 0 {
		t.Fatalf("R sent %d calls before confirmation", got)
	}
	l.turn(t, keyRunes('n'))
	l.settle(t)
	if got := b.count("workers:restart"); got != 0 {
		t.Errorf("cancelled restart sent %d workers:restart calls, want 0", got)
	}
	if l.m.views[0].(*healthView).confirm.open() {
		t.Error("n left the confirmation open")
	}
}

// y sends exactly one workers:restart, for the worker that was selected when
// the prompt opened — not for whatever the cursor has drifted to since.
func TestOverviewRestartConfirmedSendsOneCallForTheFrozenWorker(t *testing.T) {
	b, l := newHydratedSession(t)
	h := l.m.views[0].(*healthView)
	h.table.SetCursor(1) // "errors" is the restart target
	l.turn(t, keyRunes('R'))
	// The cursor moves after the prompt opened: a lazy implementation would
	// read the selection when y is pressed.
	h.table.SetCursor(4)
	l.turn(t, keyRunes('y'))
	l.until(t, func() bool { return len(b.restartParams()) == 1 })
	l.settle(t)
	sent := b.restartParams()
	if len(sent) != 1 {
		t.Fatalf("workers:restart sent %d times, want exactly 1: %v", len(sent), sent)
	}
	if sent[0]["name"] != "errors" {
		t.Errorf("restarted %q, want the worker frozen at prompt time (%q)", sent[0]["name"], "errors")
	}
	if h.confirm.open() {
		t.Error("confirmation still open after y")
	}
	// A repeated y must not repeat the request.
	l.turn(t, keyRunes('y'))
	l.settle(t)
	if got := b.count("workers:restart"); got != 1 {
		t.Errorf("a second y sent %d workers:restart calls in total, want 1", got)
	}
}

// While the confirmation is open the prompt owns the keyboard: the global quit
// and tab-switch bindings must not fire, or a restart could be bypassed (or
// the program exited) without an answer.
func TestOverviewOpenConfirmationBlocksGlobalKeys(t *testing.T) {
	b, l := newHydratedSession(t)
	l.turn(t, keyRunes('R'))
	if !l.m.views[0].(*healthView).confirm.open() {
		t.Fatal("no confirmation to test")
	}
	for _, k := range []tea.KeyMsg{keyRunes('q'), {Type: tea.KeyTab}, {Type: tea.KeyShiftTab}, {Type: tea.KeyCtrlC}, keyRunes('?'), keyRunes('l'), keyRunes('h')} {
		next, cmd := l.m.Update(k)
		m, ok := next.(Model)
		if !ok {
			t.Fatalf("model %T", next)
		}
		l.m = m
		if cmd != nil {
			if msg := cmd(); msg != nil {
				if _, quit := msg.(tea.QuitMsg); quit {
					t.Fatalf("%v quit the program while a confirmation was open", k)
				}
			}
		}
		if l.m.active != 0 {
			t.Fatalf("%v switched tabs while a confirmation was open", k)
		}
		if !l.m.views[0].(*healthView).confirm.open() {
			t.Fatalf("%v answered the confirmation without y or n", k)
		}
	}
	if got := b.count("workers:restart"); got != 0 {
		t.Errorf("blocked keys still sent %d workers:restart calls", got)
	}
}

func TestOverviewPushDuringRecoveryWinsOverHeldWorkerSnapshot(t *testing.T) {
	b, l := newRecoverySession(t)
	entered := b.hold("workers:get-status")
	b.push(t, "A", 1, "heartbeat", map[string]any{})
	l.until(t, func() bool { return l.m.recoveryActive })
	awaitEntry(t, entered)
	b.push(t, "A", 2, "workers:update", map[string]any{"workers": []workerStatus{
		{Name: "scheduler", State: "exhausted", Attempts: 5, LastError: "probe timed out"},
	}})
	h := l.m.views[0].(*healthView)
	l.until(t, func() bool { return len(h.workers) == 1 })
	b.unhold()
	l.until(t, func() bool { return l.m.hydratedEpoch == "A" })
	if len(h.workers) != 1 || h.workers[0].Name != "scheduler" || h.workers[0].State != "exhausted" {
		t.Fatalf("stale recovery snapshot replaced newer worker facts: %v", h.workers)
	}
}

func TestOverviewWorkerGetterFailureDoesNotBlockOtherDomains(t *testing.T) {
	b, l := newRecoverySession(t)
	b.mu.Lock()
	b.fail = "workers:get-status"
	b.mu.Unlock()
	b.push(t, "A", 1, "heartbeat", map[string]any{})
	l.until(t, func() bool { return l.m.hydratedEpoch == "A" })
	h := l.m.views[0].(*healthView)
	if len(h.workers) != 0 || !strings.Contains(h.status, "load worker status failed") {
		t.Fatalf("failed worker getter invented health or lost its error: workers=%v status=%q", h.workers, h.status)
	}
	if id := l.m.views[6].(*clusterView).identity.NodeUUID; id != "A" {
		t.Fatalf("worker getter failure blocked identity recovery: %q", id)
	}
}

func TestOverviewRestartFailureIsReported(t *testing.T) {
	b, l := newHydratedSession(t)
	b.mu.Lock()
	b.fail = "workers:restart"
	b.mu.Unlock()
	l.turn(t, keyRunes('R'))
	l.turn(t, keyRunes('y'))
	h := l.m.views[0].(*healthView)
	l.until(t, func() bool { return strings.Contains(h.status, "restart scanner failed") })
	if got := b.count("workers:restart"); got != 1 {
		t.Fatalf("failed restart sent %d requests", got)
	}
}

func TestErrorsSnapshotStillUpdatesErrorsTabNotWorkerHealth(t *testing.T) {
	b, l := newHydratedSession(t)
	b.push(t, "A", 2, "errors:update", []svcerrors.ServiceError{
		{ID: "supervisor:subprocess-crashed:scanner", Message: "scanner crashed", NodeID: "A"},
	})
	e := l.m.views[1].(*errorsView)
	l.until(t, func() bool { return len(e.errs) == 1 })
	if row := e.table.Rows()[0]; row[3] != "scanner crashed" {
		t.Fatalf("Errors tab lost the error snapshot: %v", row)
	}
	if state := l.m.views[0].(*healthView).table.Rows()[0][1]; state != "running" {
		t.Fatalf("Overview derived health from crash errors: %q", state)
	}
}
