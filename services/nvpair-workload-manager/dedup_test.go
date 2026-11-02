// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// emitDedupKey exercises the production dedup path with a successful broker
// emit, then reports whether that key was already recorded.
func emitDedupKey(t *testing.T, d *dedupIndex, key string) bool {
	t.Helper()
	duplicate, err := d.emitOnce(key, func() error { return nil })
	if err != nil {
		t.Fatalf("emit dedup key: %v", err)
	}
	return duplicate
}

// TestDedupIndex_EmitOnceSerializesOnlyMatchingKeys proves that an in-flight
// emit blocks only requests with the same key. The test controls this order:
//
//  1. The "first" goroutine reserves its key, enters its emit callback, and
//     signals firstEntered. It then waits on releaseFirst, keeping that key
//     in flight.
//  2. While "first" is still waiting, the "second" goroutine emits a
//     different key. The test requires secondDone before closing releaseFirst.
//  3. The test closes releaseFirst and waits for the original emit to finish.
//
// A single lock held across all emit callbacks would make step 2 time out.
func TestDedupIndex_EmitOnceSerializesOnlyMatchingKeys(t *testing.T) {
	d := newDedupIndex(10)
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	// Release the blocked goroutine even if an assertion fails early.
	defer func() {
		select {
		case <-releaseFirst:
		default:
			close(releaseFirst)
		}
	}()
	firstDone := make(chan error, 1)
	go func() {
		_, err := d.emitOnce("first", func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
		firstDone <- err
	}()
	select {
	case <-firstEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("first key did not begin emitting")
	}

	type result struct {
		duplicate bool
		err       error
	}
	secondDone := make(chan result, 1)
	go func() {
		duplicate, err := d.emitOnce("second", func() error { return nil })
		secondDone <- result{duplicate: duplicate, err: err}
	}()
	select {
	case got := <-secondDone:
		if got.err != nil || got.duplicate {
			t.Fatalf("different key result = (%t, %v), want (false, nil)", got.duplicate, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("different key blocked behind the first emit")
	}
	close(releaseFirst)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first key emit: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first key did not finish")
	}
}

// TestDedupIndex_WaiterRetriesAfterFailedEmit proves that a failed emit does
// not permanently reserve or record its key. Unlike the different-key test
// above, both goroutines use "same":
//
//  1. The first goroutine reserves "same", enters its emit callback, signals
//     firstEntered, and waits on releaseFirst.
//  2. The second goroutine requests "same". It must wait while the first
//     callback is in flight; returning or emitting before releaseFirst closes
//     fails the test.
//  3. The test closes releaseFirst. The first callback returns an error, so
//     "same" remains unrecorded. The waiting goroutine then emits it once and
//     succeeds.
//
// The final retry count distinguishes a real retry from a duplicate response.
func TestDedupIndex_WaiterRetriesAfterFailedEmit(t *testing.T) {
	d := newDedupIndex(10)
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	defer func() {
		select {
		case <-releaseFirst:
		default:
			close(releaseFirst)
		}
	}()
	wantErr := errors.New("broker unavailable")
	firstDone := make(chan error, 1)
	go func() {
		_, err := d.emitOnce("same", func() error {
			close(firstEntered)
			<-releaseFirst
			return wantErr
		})
		firstDone <- err
	}()
	select {
	case <-firstEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not begin emitting")
	}

	var retries atomic.Int32
	secondDone := make(chan error, 1)
	go func() {
		duplicate, err := d.emitOnce("same", func() error {
			retries.Add(1)
			return nil
		})
		if duplicate {
			secondDone <- errors.New("failed key was treated as a duplicate")
			return
		}
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("waiting request completed before first emit: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	select {
	case err := <-firstDone:
		if !errors.Is(err, wantErr) {
			t.Fatalf("first emit error = %v, want %v", err, wantErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not finish")
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("waiting retry: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiting retry did not finish")
	}
	if got := retries.Load(); got != 1 {
		t.Fatalf("retry emits = %d, want 1", got)
	}
}

func TestDedupEmitOnce(t *testing.T) {
	d := newDedupIndex(8)

	if emitDedupKey(t, d, "a") {
		t.Fatal("first sighting of a should be new")
	}
	if !emitDedupKey(t, d, "a") {
		t.Fatal("second sighting of a should be a duplicate")
	}
	if emitDedupKey(t, d, "b") {
		t.Fatal("first sighting of b should be new")
	}
}

func TestDedupEviction(t *testing.T) {
	d := newDedupIndex(2)

	emitDedupKey(t, d, "a") // {a}
	emitDedupKey(t, d, "b") // {a,b}
	emitDedupKey(t, d, "c") // evicts a -> {b,c}

	if emitDedupKey(t, d, "a") {
		t.Fatal("a should have been evicted and read as new")
	}
}

func TestDedupKeysDistinguishStateAndKind(t *testing.T) {
	d := newDedupIndex(8)

	w := &Workload{ID: "wl-1", OriginatedFrom: "node-A", State: StateQueued}
	wRunning := &Workload{ID: "wl-1", OriginatedFrom: "node-A", State: StateRunning}

	if emitDedupKey(t, d, keyLifecycle(w)) {
		t.Fatal("queued should be new")
	}
	if emitDedupKey(t, d, keyLifecycle(wRunning)) {
		t.Fatal("running for same id is a different key, should be new")
	}
	if !emitDedupKey(t, d, keyLifecycle(w)) {
		t.Fatal("repeat queued should dedup")
	}
	// A removal keyed on the same id must not collide with a lifecycle key.
	if emitDedupKey(t, d, keyRemove("node-A", "wl-1")) {
		t.Fatal("removal of wl-1 must not collide with lifecycle keys")
	}
}

// TestDedupDistinguishesNodes guards the cross-node collision: Workload.id is
// only unique per node (spec §11), so the same id+state from two different
// nodes must be treated as two distinct workloads, never deduplicated against
// each other.
func TestDedupDistinguishesNodes(t *testing.T) {
	d := newDedupIndex(8)

	nodeA := &Workload{ID: "wl-1", OriginatedFrom: "node-A", State: StateQueued}
	nodeB := &Workload{ID: "wl-1", OriginatedFrom: "node-B", State: StateQueued}

	if emitDedupKey(t, d, keyLifecycle(nodeA)) {
		t.Fatal("node-A wl-1 should be new")
	}
	if emitDedupKey(t, d, keyLifecycle(nodeB)) {
		t.Fatal("node-B wl-1 has the same id but a different node, must not dedup against node-A")
	}
	if !emitDedupKey(t, d, keyLifecycle(nodeA)) {
		t.Fatal("repeat of node-A wl-1 should dedup")
	}
}

// TestDedupDistinguishesEngineAndRun guards the identity fix: id "1" is reused
// by the two engine proxies (each counts from 1) and after a restart (new
// runId). The dedup must treat those as distinct workloads, not collapse them.
func TestDedupDistinguishesEngineAndRun(t *testing.T) {
	d := newDedupIndex(8)
	ollama := &Workload{ID: "1", OriginatedFrom: "host", Engine: "ollama", RunID: "r1", State: StateRunning}
	lmstudio := &Workload{ID: "1", OriginatedFrom: "host", Engine: "lmstudio", RunID: "r2", State: StateRunning}
	restarted := &Workload{ID: "1", OriginatedFrom: "host", Engine: "ollama", RunID: "r3", State: StateRunning}

	if emitDedupKey(t, d, keyLifecycle(ollama)) {
		t.Fatal("ollama host/1 should be new")
	}
	if emitDedupKey(t, d, keyLifecycle(lmstudio)) {
		t.Fatal("lmstudio host/1 shares the id but a different engine; must not dedup")
	}
	if emitDedupKey(t, d, keyLifecycle(restarted)) {
		t.Fatal("a reused id from a new run must not dedup against the old run")
	}
	if !emitDedupKey(t, d, keyLifecycle(ollama)) {
		t.Fatal("repeat of ollama host/1 should dedup")
	}
}

// TestDedupRemovalDistinguishesNodes mirrors TestDedupDistinguishesNodes for
// the removal path: the same workloadId removed on two nodes must be two
// distinct dedup entries.
func TestDedupRemovalDistinguishesNodes(t *testing.T) {
	d := newDedupIndex(8)

	if emitDedupKey(t, d, keyRemove("node-A", "wl-1")) {
		t.Fatal("removal of node-A wl-1 should be new")
	}
	if emitDedupKey(t, d, keyRemove("node-B", "wl-1")) {
		t.Fatal("removal of node-B wl-1 must not dedup against node-A")
	}
	if !emitDedupKey(t, d, keyRemove("node-A", "wl-1")) {
		t.Fatal("repeat removal of node-A wl-1 should dedup")
	}
}
