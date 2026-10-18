// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	serviceerrors "nvpair-shared/errors"
	"nvpair-shared/workerhealth"
)

func TestWorkersSnapshotOrderAndEligibility(t *testing.T) {
	b := NewBroker(NewCodec(writeOnlyReadWriter{&bytes.Buffer{}}), workerPaths{})
	want := []string{"scanner", "errors", "settings", "engine-manager", "node-info", "proxy", "lmstudio-proxy", "omlx-proxy", "workload-manager", "manual-nodes", "cluster-manager", "scheduler"}
	got := b.workerSnapshot().Workers
	if len(got) != len(want) {
		t.Fatalf("got %d workers", len(got))
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Fatalf("worker %d = %s, want %s", i, got[i].Name, name)
		}
		eligible := name != "engine-manager" && name != "proxy" && name != "lmstudio-proxy" && name != "omlx-proxy"
		policy := b.workerEntries[i].sup.hang.withDefaults()
		if policy.interval != 30*time.Second || policy.deadline != 5*time.Second || policy.degradeAfter != 2 || policy.restartAfter != 3 {
			t.Fatalf("policy = %+v", policy)
		}
		if got[i].RestartOnHang != eligible {
			t.Fatalf("eligibility: %+v", got[i])
		}
	}
}

func TestWorkersMissingRestartAndNotifications(t *testing.T) {
	var out bytes.Buffer
	b := NewBroker(NewCodec(writeOnlyReadWriter{&out}), workerPaths{})
	entry := b.workerEntries[9] // manual-nodes
	entry.expectedPath = filepath.Join(t.TempDir(), "nvpair-manual-nodes")
	h := newFakeHandle()
	entry.sup.spawn = func() (supervisedHandle, error) { return h, nil }
	before := b.workerSnapshot().Workers[9]
	if before.State != WorkerMissing || !strings.Contains(before.LastError, "nvpair-manual-nodes") {
		t.Fatalf("missing: %+v", before)
	}
	unchanged, rpcErr := b.restartWorker("manual-nodes")
	if rpcErr != nil || unchanged != before {
		t.Fatalf("absent restart changed state: %+v, %v", unchanged, rpcErr)
	}
	if err := os.WriteFile(entry.expectedPath, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, rpcErr := b.restartWorker("manual-nodes"); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	defer entry.sup.Stop()
	deadline := time.After(time.Second)
	for b.workerSnapshot().Workers[9].State != WorkerRunning {
		select {
		case <-deadline:
			t.Fatal("never running")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	// Join callbacks before inspecting the output buffer.
	entry.sup.Stop()
	dec := json.NewDecoder(&out)
	states := []WorkerState{}
	for dec.More() {
		var msg Message
		if err := dec.Decode(&msg); err != nil {
			t.Fatal(err)
		}
		if msg.Method != "workers:update" {
			t.Fatalf("unexpected method: %s", msg.Method)
		}
		var snapshot WorkersResult
		if err := json.Unmarshal(msg.Params, &snapshot); err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Workers) != 12 {
			t.Fatal("not a full snapshot")
		}
		states = append(states, snapshot.Workers[9].State)
	}
	if len(states) != 2 || states[0] != WorkerRestarting || states[1] != WorkerRunning {
		t.Fatalf("updates = %v", states)
	}
}

func TestWorkersAuxiliaryInitialFailureRetries(t *testing.T) {
	b := NewBroker(NewCodec(writeOnlyReadWriter{io.Discard}), workerPaths{settings: "fixture"})
	entry := b.workerEntries[2]
	entry.sup.policy = fastPolicy(5, time.Hour)
	calls := 0
	entry.sup.spawn = func() (supervisedHandle, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("transient spawn failure")
		}
		return newFakeHandle(), nil
	}
	b.startWorker("settings")
	defer entry.sup.Stop()
	deadline := time.After(time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		status := b.workerSnapshot().Workers[2]
		if status.State == WorkerRunning && status.Attempts == 1 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("initial spawn was not retried: %+v", status)
		case <-tick.C:
		}
	}
}

func TestWorkersRPCSurface(t *testing.T) {
	for _, params := range []string{`{}`, `{"name":123}`, `{"name":"unknown"}`, `null`} {
		var out bytes.Buffer
		b := NewBroker(NewCodec(writeOnlyReadWriter{&out}), workerPaths{})
		id := json.RawMessage(`1`)
		b.handleMessage(&Message{JSONRPC: "2.0", ID: &id, Method: "workers:restart", Params: json.RawMessage(params)})
		var msg Message
		if err := json.Unmarshal(out.Bytes(), &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Error == nil || msg.Error.Code != -32602 {
			t.Fatalf("params %s: %+v", params, msg)
		}
	}
	var out bytes.Buffer
	b := NewBroker(NewCodec(writeOnlyReadWriter{&out}), workerPaths{})
	id := json.RawMessage(`2`)
	b.handleMessage(&Message{JSONRPC: "2.0", ID: &id, Method: "workers:get-status"})
	var msg Message
	if err := json.Unmarshal(out.Bytes(), &msg); err != nil {
		t.Fatal(err)
	}
	var result WorkersResult
	if err := json.Unmarshal(msg.Result, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Workers) != 12 {
		t.Fatalf("get-status: %+v", msg)
	}
}

type blockingWorkerStop struct {
	*fakeHandle
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *blockingWorkerStop) Stop() { h.once.Do(func() { close(h.entered) }); <-h.release }

func TestWorkersRestartDoesNotWaitForStop(t *testing.T) {
	for _, name := range []string{"scanner", "scheduler"} {
		t.Run(name, func(t *testing.T) {
			b := NewBroker(NewCodec(writeOnlyReadWriter{io.Discard}), workerPaths{scanner: "fixture", scheduler: "fixture"})
			var entry *workerEntry
			for _, e := range b.workerEntries {
				if e.sup.name == name {
					entry = e
				}
			}
			h := &blockingWorkerStop{fakeHandle: newFakeHandle(), entered: make(chan struct{}), release: make(chan struct{})}
			entry.sup.spawn = func() (supervisedHandle, error) { return h, nil }
			entry.sup.StartRetrying()
			defer entry.sup.Stop()
			defer close(h.release)
			entry.started = true
			returned := make(chan struct{})
			go func() { defer close(returned); _, _ = b.restartWorker(name) }()
			select {
			case <-returned:
			case <-time.After(time.Second):
				t.Fatal("restart blocked the client loop")
			}
			select {
			case <-h.entered:
			case <-time.After(time.Second):
				t.Fatal("restart not initiated")
			}
		})
	}
}

func TestWorkersMissingRestartDoesNotWaitForSpawn(t *testing.T) {
	b := NewBroker(NewCodec(writeOnlyReadWriter{io.Discard}), workerPaths{})
	entry := b.workerEntries[9]
	entry.expectedPath = filepath.Join(t.TempDir(), "nvpair-manual-nodes")
	if err := os.WriteFile(entry.expectedPath, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	entered := make(chan struct{})
	entry.sup.spawn = func() (supervisedHandle, error) { close(entered); <-release; return newFakeHandle(), nil }
	defer func() { close(release); entry.sup.Stop() }()
	returned := make(chan struct{})
	go func() { defer close(returned); _, _ = b.restartWorker("manual-nodes") }()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("missing restart blocked on initialization")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("spawn not initiated")
	}
}

func TestWorkerHealthResponseCorrelation(t *testing.T) {
	for _, kind := range []string{"node-info", "workload-manager"} {
		t.Run(kind, func(t *testing.T) {
			parent, child := net.Pipe()
			defer parent.Close()
			defer child.Close()
			server := NewPeer(NewCodec(child))
			go server.Serve(func(msg *Message) {
				if msg.Method != workerhealth.Method {
					t.Errorf("method = %s", msg.Method)
				}
				_ = server.Respond(msg.ID, workerhealth.OK())
			}, nil)
			var health func(context.Context) error
			if kind == "node-info" {
				n := &nodeInfoProcess{peer: nodeInfoPeer(parent, parent)}
				go n.peer.Serve(nil, nil)
				health = n.Health
			} else {
				m := &workloadManagerProcess{stdin: parent, peer: NewPeer(NewCodec(parent))}
				go m.readMessages()
				health = m.Health
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := health(ctx); err != nil {
				t.Fatalf("health response dropped: %v", err)
			}
		})
	}
}

func TestWorkersCrashReason(t *testing.T) {
	for _, reason := range []string{"unresponsive", "exited unexpectedly"} {
		t.Run(reason, func(t *testing.T) {
			b := NewBroker(NewCodec(writeOnlyReadWriter{io.Discard}), workerPaths{})
			var sink bytes.Buffer
			b.setErrors(&errorsProcess{peer: NewPeer(NewCodec(writeOnlyReadWriter{&sink}))})
			status := b.workerSnapshot().Workers[11]
			status.State, status.LastError = WorkerRestarting, reason
			b.updateWorker(status)
			crash, _ := b.crashReporter("scheduler")
			crash(1)
			var msg Message
			if err := json.Unmarshal(sink.Bytes(), &msg); err != nil {
				t.Fatal(err)
			}
			var report serviceerrors.ServiceError
			if err := json.Unmarshal(msg.Params, &report); err != nil {
				t.Fatal(err)
			}
			want := "subprocess scheduler exited unexpectedly"
			if reason == "unresponsive" {
				want = "subprocess scheduler stopped responding"
			}
			if report.ID != "supervisor:subprocess-crashed:scheduler" || report.Message != want {
				t.Fatalf("report = %+v", report)
			}
		})
	}
}

func TestWorkersUnknownRestart(t *testing.T) {
	b := NewBroker(NewCodec(writeOnlyReadWriter{&bytes.Buffer{}}), workerPaths{})
	_, rpcErr := b.restartWorker("not-a-worker")
	if rpcErr == nil || rpcErr.Code != -32602 {
		t.Fatalf("error = %v", rpcErr)
	}
}

func TestWorkersStateChangeDeduplicates(t *testing.T) {
	var out bytes.Buffer
	b := NewBroker(NewCodec(writeOnlyReadWriter{&out}), workerPaths{})
	status := b.workerSnapshot().Workers[0]
	status.State, status.Since = WorkerRunning, time.Now()
	b.updateWorker(status)
	b.updateWorker(status)
	if count := strings.Count(out.String(), `"method":"workers:update"`); count != 1 {
		t.Fatalf("got %d updates", count)
	}
}
