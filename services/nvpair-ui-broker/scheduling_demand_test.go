// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"

	"nvpair-shared/noderec"
	"nvpair-ui-broker/relay"
	"nvpair-ui-broker/workloadstore"
)

// schedulingDemandSink stands in for the two workers that own an outbound
// node-info poll: the scanner's daemon and manual-nodes. Both are told the same
// thing, so a test reads both and fails if they ever disagree.
type schedulingDemandSink struct {
	scanner *lockedBuffer
	manual  *lockedBuffer
	b       *Broker
}

func newSchedulingDemandSink() *schedulingDemandSink {
	s := &schedulingDemandSink{scanner: &lockedBuffer{}, manual: &lockedBuffer{}}
	s.b = &Broker{workloads: workloadstore.New(), nodeID: "self"}
	s.b.setScanner(&scannerProcess{peer: NewPeer(NewCodec(writeOnlyReadWriter{s.scanner}))})
	s.b.setManualNodes(&rpcWorker{peer: NewPeer(NewCodec(writeOnlyReadWriter{s.manual}))})
	return s
}

// frames returns the demand pushes each worker received since the last call, as
// one list per worker. Both frames on the wire must carry the same method.
func (s *schedulingDemandSink) frames(t *testing.T) (scanner []bool, manual []bool) {
	t.Helper()
	return s.decode(t, s.scanner), s.decode(t, s.manual)
}

func (s *schedulingDemandSink) decode(t *testing.T, buf *lockedBuffer) []bool {
	t.Helper()
	return decodeDemandFrames(t, buf)
}

// decodeDemandFrames parses the scheduling-demand notifications a worker
// received since the buffer was last reset, one bool per frame.
func decodeDemandFrames(t *testing.T, buf *lockedBuffer) []bool {
	t.Helper()
	raw := buf.String()
	buf.Reset()
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var active []bool
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		var frame struct {
			Method string                         `json:"method"`
			Params noderec.SchedulingDemandParams `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("decode frame %q: %v", line, err)
		}
		if frame.Method != noderec.MethodSetSchedulingDemand {
			t.Fatalf("method = %q, want %q", frame.Method, noderec.MethodSetSchedulingDemand)
		}
		active = append(active, frame.Params.Active)
	}
	return active
}

// demand reports both workers agreed on exactly these pushes, and clears them.
func (s *schedulingDemandSink) demand(t *testing.T, want ...bool) {
	t.Helper()
	scanner, manual := s.frames(t)
	if !slices.Equal(scanner, want) {
		t.Fatalf("scanner received %v, want %v", scanner, want)
	}
	if !slices.Equal(manual, want) {
		t.Fatalf("manual-nodes received %v, want %v", manual, want)
	}
}

func upsertJSON(t *testing.T, fields map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"workloadInfo": fields})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// The scanner and manual-nodes poll every node every couple of seconds whether
// or not anything is scheduled, so what they renew is decided here. A queued or
// running workload anywhere in the store — this node's or a peer's, which the
// scheduler must weigh — is scheduling demand; nothing non-terminal anywhere means
// no scheduler is waiting, and every node may drop to idle sampling.
func TestSchedulingDemandFollowsWorkloadTransitions(t *testing.T) {
	s := newSchedulingDemandSink()

	s.b.emitWorkloadEvent("workloads:upsert", upsertJSON(t, map[string]any{
		"id":             "w1",
		"originatedFrom": "peer-node",
		"state":          "queued",
		"createdAt":      100,
	}))
	// Queued and unscheduled still counts: the scheduler has work to place, and
	// placing it is what it needs fresh telemetry for.
	s.demand(t, true)

	// The origin's anti-entropy heartbeat re-asserts the same state. It is not a
	// transition, so it must not re-push.
	s.b.emitWorkloadEvent("workloads:upsert", upsertJSON(t, map[string]any{
		"id":             "w1",
		"originatedFrom": "peer-node",
		"state":          "queued",
		"createdAt":      100,
	}))
	s.demand(t)

	// A second workload appears; demand was already on.
	s.b.emitWorkloadEvent("workloads:upsert", upsertJSON(t, map[string]any{
		"id":             "w2",
		"originatedFrom": "self",
		"state":          "running",
		"scheduledOn":    "self",
		"createdAt":      200,
	}))
	s.demand(t)

	// One of the two finishes; the other still needs telemetry.
	s.b.emitWorkloadEvent("workloads:upsert", upsertJSON(t, map[string]any{
		"id":             "w1",
		"originatedFrom": "peer-node",
		"state":          "completed",
		"createdAt":      100,
		"completedAt":    300,
	}))
	s.demand(t)

	// The last one finishes: nothing is queued or running anywhere, so the
	// workers go back to peeking.
	s.b.emitWorkloadEvent("workloads:upsert", upsertJSON(t, map[string]any{
		"id":             "w2",
		"originatedFrom": "self",
		"state":          "failed",
		"scheduledOn":    "self",
		"createdAt":      200,
		"completedAt":    400,
	}))
	s.demand(t, false)
}

// A workload that only ever reports a terminal state (a late join, or a replay)
// leaves demand where it was rather than pushing a redundant frame.
func TestSchedulingDemandIgnoresTerminalOnlyTraffic(t *testing.T) {
	s := newSchedulingDemandSink()

	s.b.emitWorkloadEvent("workloads:upsert", upsertJSON(t, map[string]any{
		"id":             "done",
		"originatedFrom": "peer-node",
		"state":          "completed",
		"createdAt":      100,
		"completedAt":    200,
	}))
	s.demand(t)
}

// A removed workload frees its slot, so removing the last active one releases
// scheduling demand like any terminal does.
func TestSchedulingDemandReleasedByWorkloadRemoval(t *testing.T) {
	s := newSchedulingDemandSink()

	s.b.emitWorkloadEvent("workloads:upsert", upsertJSON(t, map[string]any{
		"id":             "w1",
		"originatedFrom": "peer-node",
		"state":          "running",
		"createdAt":      100,
	}))
	s.demand(t, true)

	s.b.emitWorkloadEvent("workloads:remove", json.RawMessage(`{"originatedFrom":"peer-node","workloadID":"w1"}`))
	s.demand(t, false)
}

// A (re)spawned worker knows nothing, so its default must be background until
// told otherwise — and the spawn push must carry the state the store already
// holds, not a hardcoded "inactive".
func TestSchedulingDemandPushedOnRespawn(t *testing.T) {
	s := newSchedulingDemandSink()
	raw := upsertJSON(t, map[string]any{
		"id":             "w1",
		"originatedFrom": "peer-node",
		"state":          "running",
		"createdAt":      100,
	})
	in, ok := workloadstore.ParseIncoming(mustInnerWorkloadInfo(t, raw))
	if !ok {
		t.Fatal("parse failed")
	}
	s.b.workloads.Apply(in)

	s.b.pushSchedulingDemandOnStart()
	s.demand(t, true)

	empty := newSchedulingDemandSink()
	empty.b.pushSchedulingDemandOnStart()
	empty.demand(t, false)
}

// mustInnerWorkloadInfo unwraps a workloads:upsert envelope into the
// workloadInfo object ParseIncoming expects.
func mustInnerWorkloadInfo(t *testing.T, envelope json.RawMessage) json.RawMessage {
	t.Helper()
	var decoded struct {
		WorkloadInfo json.RawMessage `json:"workloadInfo"`
	}
	if err := json.Unmarshal(envelope, &decoded); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	return decoded.WorkloadInfo
}

// gateWriter parks the first frame written through it until released, so a test
// can inspect the Broker while a demand push is mid-flight.
type gateWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	inner   *lockedBuffer
}

func (w *gateWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return w.inner.Write(p)
}

// fakeCmd is the minimum a handle needs for the spawn paths' log line, which
// reports the child PID.
func fakeCmd() *exec.Cmd {
	return &exec.Cmd{Process: &os.Process{}}
}

// applyRunningWorkload seeds the store with one active peer-origin workload,
// which is scheduling demand that the spawn push must report.
func applyRunningWorkload(t *testing.T, b *Broker) {
	t.Helper()
	raw := upsertJSON(t, map[string]any{
		"id":             "w1",
		"originatedFrom": "peer-node",
		"state":          "running",
		"createdAt":      100,
	})
	in, ok := workloadstore.ParseIncoming(mustInnerWorkloadInfo(t, raw))
	if !ok {
		t.Fatal("parse failed")
	}
	b.workloads.Apply(in)
}

// The spawn push reads the store, records the value, and pushes it. A workload
// transition does the same under workloadEmitMu. If the spawn side were not
// serialized with it, a transition that lands between the spawn's read and its
// Store would be overwritten by the stale read: the workers would be told
// "inactive" while work is active, and unchanged anti-entropy heartbeats return
// before recompute, so nothing would repair it. Parking the push window (the
// tail of the critical section) and proving the emit lock is still held shows the
// whole read/Store/push sequence is one critical section.
func TestSchedulingDemandSpawnPushSerializesWithTransitions(t *testing.T) {
	b := &Broker{workloads: workloadstore.New(), nodeID: "self"}
	applyRunningWorkload(t, b)
	gate := &gateWriter{entered: make(chan struct{}), release: make(chan struct{}), inner: &lockedBuffer{}}
	b.setScanner(&scannerProcess{peer: NewPeer(NewCodec(writeOnlyReadWriter{gate}))})

	pushed := make(chan struct{})
	go func() {
		b.pushSchedulingDemandOnStart()
		close(pushed)
	}()
	<-gate.entered

	if b.workloadEmitMu.TryLock() {
		b.workloadEmitMu.Unlock()
		close(gate.release)
		<-pushed
		t.Fatal("spawn demand push runs outside workloadEmitMu; a workload transition can interleave with its read/Store and leave the workers stale")
	}

	close(gate.release)
	<-pushed
	if got := decodeDemandFrames(t, gate.inner); !slices.Equal(got, []bool{true}) {
		t.Fatalf("scanner received %v, want [true]", got)
	}
}

// Both spawn paths reset a worker that knows nothing, so both must re-assert
// demand. They are driven here through their launch seams, with fake handles, so
// deleting either spawn-path call — not just the shared push — fails.
func TestScannerSpawnReassertsSchedulingDemand(t *testing.T) {
	sink := &lockedBuffer{}
	b := &Broker{workloads: workloadstore.New(), nodeID: "self", regCache: relay.NewRegistrationCache()}
	applyRunningWorkload(t, b)
	original := launchScannerProcess
	launchScannerProcess = func(*Broker) (*scannerProcess, error) {
		return &scannerProcess{peer: NewPeer(NewCodec(writeOnlyReadWriter{sink})), cmd: fakeCmd()}, nil
	}
	t.Cleanup(func() { launchScannerProcess = original })

	if _, err := b.spawnScanner(); err != nil {
		t.Fatalf("spawnScanner: %v", err)
	}
	if got := decodeDemandFrames(t, sink); !slices.Equal(got, []bool{true}) {
		t.Fatalf("scanner received %v after spawn, want [true]", got)
	}
}

func TestManualNodesSpawnReassertsSchedulingDemand(t *testing.T) {
	sink := &lockedBuffer{}
	b := &Broker{workloads: workloadstore.New(), nodeID: "self"}
	applyRunningWorkload(t, b)
	original := launchManualNodesProcess
	launchManualNodesProcess = func(*Broker) (*rpcWorker, error) {
		return &rpcWorker{peer: NewPeer(NewCodec(writeOnlyReadWriter{sink})), cmd: fakeCmd()}, nil
	}
	t.Cleanup(func() { launchManualNodesProcess = original })

	if _, err := b.spawnManualNodes(); err != nil {
		t.Fatalf("spawnManualNodes: %v", err)
	}
	if got := decodeDemandFrames(t, sink); !slices.Equal(got, []bool{true}) {
		t.Fatalf("manual-nodes received %v after spawn, want [true]", got)
	}
}
