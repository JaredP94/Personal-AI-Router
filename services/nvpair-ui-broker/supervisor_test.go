// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHandle is a test double for supervisedHandle: Done() exposes a
// channel the test closes to simulate an unexpected exit, and Stop()
// records that the supervisor tore it down.
type fakeHandle struct {
	done     chan struct{}
	stopOnce sync.Once
	stoppedC chan struct{}
}

func newFakeHandle() *fakeHandle {
	return &fakeHandle{done: make(chan struct{}), stoppedC: make(chan struct{})}
}

func (h *fakeHandle) Done() <-chan struct{} { return h.done }

func (h *fakeHandle) Stop() { h.stopOnce.Do(func() { close(h.stoppedC) }) }

// crash closes the handle's done channel, simulating an unexpected exit.
func (h *fakeHandle) crash() { close(h.done) }

func TestRestartPolicyBackoff(t *testing.T) {
	p := restartPolicy{baseDelay: time.Second, maxDelay: 16 * time.Second}
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 16 * time.Second},
		{6, 16 * time.Second}, // capped
		{100, 16 * time.Second},
	}
	for _, c := range cases {
		if got := p.backoff(c.attempt); got != c.want {
			t.Errorf("backoff(%d) = %s, want %s", c.attempt, got, c.want)
		}
	}
}

// fastPolicy is a unit-test policy: near-instant backoff so restarts don't
// slow the test, with a configurable budget and healthy-reset window.
func fastPolicy(maxAttempts int, healthyReset time.Duration) restartPolicy {
	return restartPolicy{
		baseDelay:    time.Millisecond,
		maxDelay:     5 * time.Millisecond,
		maxAttempts:  maxAttempts,
		healthyReset: healthyReset,
	}
}

func TestSupervisorSurfacesAndRestartsThenRecovers(t *testing.T) {
	spawned := make(chan *fakeHandle, 8)
	spawn := func() (supervisedHandle, error) {
		h := newFakeHandle()
		spawned <- h
		return h, nil
	}
	crashes := make(chan int, 8)
	recovered := make(chan struct{}, 8)

	// healthyReset short enough to observe recovery quickly.
	sup := newSupervisor("test", fastPolicy(5, 50*time.Millisecond), spawn)
	sup.hang = hangPolicy{probe: func(context.Context) error { return nil }, interval: time.Millisecond, deadline: time.Second}
	sup.onCrash = func(attempt int) { crashes <- attempt }
	sup.onRecovered = func() { recovered <- struct{}{} }
	if err := sup.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sup.Stop()

	h0 := mustSpawn(t, spawned)
	h0.crash()

	if got := recvInt(t, crashes); got != 1 {
		t.Fatalf("first onCrash attempt = %d, want 1", got)
	}

	// A fresh handle must be spawned (the restart).
	h1 := mustSpawn(t, spawned)

	// h1 stays up past healthyReset → onRecovered fires and the crash
	// error is cleared.
	select {
	case <-recovered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for onRecovered after a stable restart")
	}

	// Keep h1 referenced so it isn't flagged unused.
	_ = h1
}

func TestSupervisorGivesUpAfterBudget(t *testing.T) {
	spawned := make(chan *fakeHandle, 8)
	spawn := func() (supervisedHandle, error) {
		h := newFakeHandle()
		spawned <- h
		return h, nil
	}
	crashes := make(chan int, 8)

	// Budget of 2 restarts; long healthyReset so attempts never reset.
	sup := newSupervisor("test", fastPolicy(2, time.Hour), spawn)
	sup.onCrash = func(attempt int) { crashes <- attempt }
	if err := sup.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sup.Stop()

	// Initial + 2 restarts = 3 handles; the 3rd crash exhausts the budget
	// (attempt 3 > maxAttempts 2), so no 4th handle is spawned.
	for want := 1; want <= 3; want++ {
		h := mustSpawn(t, spawned)
		h.crash()
		if got := recvInt(t, crashes); got != want {
			t.Fatalf("onCrash attempt = %d, want %d", got, want)
		}
	}

	select {
	case <-spawned:
		t.Fatal("supervisor spawned a 4th worker after exhausting its restart budget")
	case <-time.After(200 * time.Millisecond):
		// No further spawn — correct.
	}
}

func TestSupervisorNoRestartPolicy(t *testing.T) {
	spawned := make(chan *fakeHandle, 8)
	spawn := func() (supervisedHandle, error) {
		h := newFakeHandle()
		spawned <- h
		return h, nil
	}
	crashes := make(chan int, 8)

	sup := newSupervisor("test", noRestartPolicy(), spawn)
	sup.onCrash = func(attempt int) { crashes <- attempt }
	if err := sup.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sup.Stop()

	h0 := mustSpawn(t, spawned)
	h0.crash()

	if got := recvInt(t, crashes); got != 1 {
		t.Fatalf("onCrash attempt = %d, want 1", got)
	}
	select {
	case <-spawned:
		t.Fatal("noRestartPolicy spawned a replacement worker")
	case <-time.After(200 * time.Millisecond):
		// No restart — correct.
	}
}

func TestSupervisorStopTearsDownCurrentHandle(t *testing.T) {
	spawned := make(chan *fakeHandle, 8)
	spawn := func() (supervisedHandle, error) {
		h := newFakeHandle()
		spawned <- h
		return h, nil
	}
	sup := newSupervisor("test", fastPolicy(5, time.Hour), spawn)
	if err := sup.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h0 := mustSpawn(t, spawned)

	done := make(chan struct{})
	go func() { sup.Stop(); close(done) }()

	select {
	case <-h0.stoppedC:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not tear down the running handle")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return")
	}
}

// TestSupervisorRestartRespawnsWithoutCrash proves the graceful restart used to
// re-point a worker at changed cluster certs on join: the running worker is
// torn down and a fresh one is spawned, but it is NOT counted as a crash (no
// onCrash, so no spurious "subprocess-crashed" error and no budget spent).
func TestSupervisorRestartRespawnsWithoutCrash(t *testing.T) {
	spawned := make(chan *fakeHandle, 8)
	spawn := func() (supervisedHandle, error) {
		h := newFakeHandle()
		spawned <- h
		return h, nil
	}
	crashes := make(chan int, 8)

	sup := newSupervisor("test", fastPolicy(5, time.Hour), spawn)
	sup.onCrash = func(attempt int) { crashes <- attempt }
	if err := sup.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sup.Stop()

	h0 := mustSpawn(t, spawned)

	// Request a graceful restart (the broker does this to re-point a worker at
	// external state it only reads at startup, e.g. an LM Studio proxy port change).
	sup.Restart()

	// The running worker is torn down...
	select {
	case <-h0.stoppedC:
	case <-time.After(2 * time.Second):
		t.Fatal("Restart did not stop the running worker")
	}
	// ...and a fresh one is spawned in its place.
	_ = mustSpawn(t, spawned)

	// A requested restart must not look like a crash.
	select {
	case a := <-crashes:
		t.Fatalf("Restart surfaced a crash (attempt %d); it must be graceful", a)
	case <-time.After(200 * time.Millisecond):
	}
}

func mustSpawn(t *testing.T, spawned <-chan *fakeHandle) *fakeHandle {
	t.Helper()
	select {
	case h := <-spawned:
		return h
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a (re)spawn")
	}
	return nil
}

func recvInt(t *testing.T, ch <-chan int) int {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting on channel")
	}
	return 0
}

// These tests drive probes through channels rather than racing the probe ticker.
// Only the policy windows themselves use real (millisecond-scale) timers.
func controlledProbe() (func(context.Context) error, <-chan chan error) {
	calls := make(chan chan error)
	return func(ctx context.Context) error {
		result := make(chan error, 1)
		select {
		case calls <- result:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case err := <-result:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}, calls
}

func replyProbe(t *testing.T, calls <-chan chan error, err error) {
	t.Helper()
	select {
	case result := <-calls:
		result <- err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for health probe")
	}
}

func receiveStatus(t *testing.T, states <-chan WorkerStatus, state WorkerState, attempts int) WorkerStatus {
	t.Helper()
	select {
	case got := <-states:
		if got.Name != "test" || got.State != state || got.Attempts != attempts || got.Since.IsZero() {
			t.Fatalf("status = %+v, want name=test state=%s attempts=%d and nonzero since", got, state, attempts)
		}
		return got
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s status", state)
		return WorkerStatus{}
	}
}

func captureStates(sup *supervisor) <-chan WorkerStatus {
	states := make(chan WorkerStatus, 128)
	sup.onState = func(status WorkerStatus) { states <- status }
	return states
}

func TestSupervisorStartFailureRemainsSynchronous(t *testing.T) {
	want := errors.New("scanner cannot start")
	var calls atomic.Int32
	sup := newSupervisor("test", fastPolicy(5, 0), func() (supervisedHandle, error) {
		calls.Add(1)
		return nil, want
	})
	states := captureStates(sup)
	if got := sup.Start(); got != want {
		t.Fatalf("Start error = %v, want original spawn error", got)
	}
	select {
	case got := <-states:
		t.Fatalf("failed Start established a monitor: %+v", got)
	case <-time.After(10 * time.Millisecond):
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("failed Start retried %d times", got)
	}
}

type pidHandle struct{ *fakeHandle }

func (*pidHandle) PID() int { return 1234 }

func TestSupervisorStatusReportsLivePID(t *testing.T) {
	h := &pidHandle{newFakeHandle()}
	sup := newSupervisor("test", noRestartPolicy(), func() (supervisedHandle, error) { return h, nil })
	states := captureStates(sup)
	if err := sup.Start(); err != nil {
		t.Fatal(err)
	}
	defer sup.Stop()
	if got := receiveStatus(t, states, "running", 0); got.PID != 1234 {
		t.Fatalf("live PID = %d, want 1234", got.PID)
	}
	h.crash()
	if got := receiveStatus(t, states, "exhausted", 1); got.PID != 0 {
		t.Fatalf("dead handle retained PID %d", got.PID)
	}
}

func TestInitialSpawnFailureRetriesWithinBudget(t *testing.T) {
	var calls atomic.Int32
	h := newFakeHandle()
	sup := newSupervisor("test", fastPolicy(5, 0), func() (supervisedHandle, error) {
		if calls.Add(1) <= 2 {
			return nil, errors.New("spawn failed")
		}
		return h, nil
	})
	states := captureStates(sup)
	sup.StartRetrying()
	defer sup.Stop()
	first := receiveStatus(t, states, "restarting", 1)
	second := receiveStatus(t, states, "restarting", 2)
	if first.LastError != "spawn failed" || second.LastError != "spawn failed" || first.PID != 0 {
		t.Fatalf("spawn error not reported: %+v, %+v", first, second)
	}
	receiveStatus(t, states, "running", 2)
	if got := calls.Load(); got != 3 {
		t.Fatalf("spawn calls = %d, want 3", got)
	}
}

func TestInitialSpawnFailureExhausts(t *testing.T) {
	var calls atomic.Int32
	sup := newSupervisor("test", fastPolicy(5, 0), func() (supervisedHandle, error) {
		calls.Add(1)
		return nil, errors.New("spawn failed")
	})
	states := captureStates(sup)
	exhausted := make(chan int, 1)
	sup.onExhausted = func(attempt int) { exhausted <- attempt }
	sup.StartRetrying()
	defer sup.Stop()
	for attempt := 1; attempt <= 5; attempt++ {
		receiveStatus(t, states, "restarting", attempt)
	}
	last := receiveStatus(t, states, "exhausted", 6)
	if last.LastError != "spawn failed" || last.PID != 0 || recvInt(t, exhausted) != 6 {
		t.Fatalf("bad exhaustion status: %+v", last)
	}
	// Wait beyond the largest backoff; an exhausted worker must stay down.
	select {
	case got := <-states:
		t.Fatalf("unexpected state after exhaustion: %+v", got)
	case <-time.After(15 * time.Millisecond):
	}
	if got := calls.Load(); got != 6 {
		t.Fatalf("spawn calls = %d, want initial + 5 = 6", got)
	}
}

func TestRetryFromExhausted(t *testing.T) {
	var repaired atomic.Bool
	h := newFakeHandle()
	policy := fastPolicy(0, 0)
	policy.baseDelay, policy.maxDelay = time.Hour, time.Hour
	sup := newSupervisor("test", policy, func() (supervisedHandle, error) {
		if !repaired.Load() {
			return nil, errors.New("broken executable")
		}
		return h, nil
	})
	states := captureStates(sup)
	sup.StartRetrying()
	defer sup.Stop()
	receiveStatus(t, states, "exhausted", 1)
	repaired.Store(true)
	sup.Retry()
	running := receiveStatus(t, states, "running", 0)
	if running.LastError != "" {
		t.Fatalf("Retry retained old error: %+v", running)
	}
}

func TestHangDegradesThenRestarts(t *testing.T) {
	spawned := make(chan *fakeHandle, 4)
	sup := newSupervisor("test", fastPolicy(5, 0), func() (supervisedHandle, error) {
		h := newFakeHandle()
		spawned <- h
		return h, nil
	})
	probe, calls := controlledProbe()
	// Leave thresholds unset to exercise the default two-failure degrade and
	// three-failure restart, while injecting only the slow production timings.
	sup.hang = hangPolicy{probe: probe, interval: time.Millisecond, deadline: time.Second, restart: true}
	states := captureStates(sup)
	crashes := make(chan int, 4)
	sup.onCrash = func(attempt int) { crashes <- attempt }
	if err := sup.Start(); err != nil {
		t.Fatal(err)
	}
	defer sup.Stop()
	old := mustSpawn(t, spawned)
	if !receiveStatus(t, states, "running", 0).RestartOnHang {
		t.Fatal("eligible worker did not report RestartOnHang")
	}
	replyProbe(t, calls, errors.New("probe failed"))
	replyProbe(t, calls, errors.New("probe failed"))
	if got := receiveStatus(t, states, "degraded", 0); got.LastError != "probe failed" {
		t.Fatalf("degraded error = %q", got.LastError)
	}
	replyProbe(t, calls, errors.New("probe failed"))
	if got := receiveStatus(t, states, "restarting", 1); got.LastError != "unresponsive" || got.PID != 0 {
		t.Fatalf("hang restart status = %+v", got)
	}
	if recvInt(t, crashes) != 1 {
		t.Fatal("hang did not consume a crash attempt")
	}
	select {
	case <-old.stoppedC:
	default:
		t.Fatal("hang restart did not stop old handle before replacement")
	}
	mustSpawn(t, spawned)
	receiveStatus(t, states, "running", 1)
}

func TestHangWithoutRestartOnlyDegrades(t *testing.T) {
	h := newFakeHandle()
	sup := newSupervisor("test", fastPolicy(5, 0), func() (supervisedHandle, error) { return h, nil })
	probe, calls := controlledProbe()
	sup.hang = hangPolicy{probe: probe, interval: time.Millisecond, deadline: time.Second, degradeAfter: 2, restartAfter: 3}
	states := captureStates(sup)
	if err := sup.Start(); err != nil {
		t.Fatal(err)
	}
	defer sup.Stop()
	if receiveStatus(t, states, "running", 0).RestartOnHang {
		t.Fatal("ineligible worker reported RestartOnHang")
	}
	for i := 0; i < 4; i++ {
		replyProbe(t, calls, errors.New("probe failed"))
		if i == 1 {
			receiveStatus(t, states, "degraded", 0)
		}
	}
	replyProbe(t, calls, nil)
	if got := receiveStatus(t, states, "running", 0); got.LastError != "" {
		t.Fatalf("probe recovery retained degraded error: %+v", got)
	}
	select {
	case <-h.stoppedC:
		t.Fatal("ineligible hung worker was stopped")
	default:
	}
	// Success clears the streak: a single later failure cannot degrade it.
	replyProbe(t, calls, errors.New("probe failed again"))
	replyProbe(t, calls, nil)
	select {
	case got := <-states:
		t.Fatalf("unexpected state after isolated failure: %+v", got)
	default:
	}
}

func TestBusyProbeSuccessKeepsRunning(t *testing.T) {
	h := newFakeHandle()
	sup := newSupervisor("test", fastPolicy(5, 0), func() (supervisedHandle, error) { return h, nil })
	probe, calls := controlledProbe()
	sup.hang = hangPolicy{probe: probe, interval: time.Millisecond, deadline: time.Second, degradeAfter: 2, restartAfter: 3, restart: true}
	states := captureStates(sup)
	if err := sup.Start(); err != nil {
		t.Fatal(err)
	}
	defer sup.Stop()
	receiveStatus(t, states, "running", 0)
	for i := 0; i < 8; i++ {
		replyProbe(t, calls, nil)
	}
	select {
	case <-h.stoppedC:
		t.Fatal("responsive busy worker was stopped")
	case got := <-states:
		t.Fatalf("responsive worker changed state: %+v", got)
	default:
	}
}

func TestRepeatedHangExhausts(t *testing.T) {
	var spawns atomic.Int32
	stops := make(chan struct{}, 8)
	sup := newSupervisor("test", fastPolicy(2, 0), func() (supervisedHandle, error) {
		spawns.Add(1)
		return &stopRecordingHandle{fakeHandle: newFakeHandle(), stops: stops}, nil
	})
	sup.hang = hangPolicy{probe: func(context.Context) error { return errors.New("wedged") }, interval: time.Millisecond, deadline: time.Second, degradeAfter: 2, restartAfter: 3, restart: true}
	states := captureStates(sup)
	if err := sup.Start(); err != nil {
		t.Fatal(err)
	}
	defer sup.Stop()
	for attempt := 0; attempt < 3; attempt++ {
		receiveStatus(t, states, "running", attempt)
		receiveStatus(t, states, "degraded", attempt)
		if attempt < 2 {
			receiveStatus(t, states, "restarting", attempt+1)
		} else {
			receiveStatus(t, states, "exhausted", 3)
		}
		select {
		case <-stops:
		case <-time.After(time.Second):
			t.Fatal("hung handle not stopped")
		}
	}
	select {
	case got := <-states:
		t.Fatalf("repeated hang did not remain exhausted: %+v", got)
	case <-time.After(15 * time.Millisecond):
	}
	if got := spawns.Load(); got != 3 {
		t.Fatalf("spawns = %d, want 3", got)
	}
}

type stopRecordingHandle struct {
	*fakeHandle
	stops chan<- struct{}
}

func (h *stopRecordingHandle) Stop() {
	h.fakeHandle.Stop()
	h.stops <- struct{}{}
}

func TestHealthyResetRequiresProbe(t *testing.T) {
	for _, probing := range []bool{false, true} {
		t.Run(map[bool]string{false: "nil-probe", true: "no-success"}[probing], func(t *testing.T) {
			spawned := make(chan *fakeHandle, 4)
			sup := newSupervisor("test", fastPolicy(5, 10*time.Millisecond), func() (supervisedHandle, error) {
				h := newFakeHandle()
				spawned <- h
				return h, nil
			})
			if probing {
				sup.hang = hangPolicy{probe: func(context.Context) error { return errors.New("not responsive") }, interval: time.Millisecond, deadline: time.Second, degradeAfter: 2}
			}
			crashes := make(chan int, 4)
			recovered := make(chan struct{}, 4)
			sup.onCrash = func(attempt int) { crashes <- attempt }
			sup.onRecovered = func() { recovered <- struct{}{} }
			if err := sup.Start(); err != nil {
				t.Fatal(err)
			}
			defer sup.Stop()
			mustSpawn(t, spawned).crash()
			if recvInt(t, crashes) != 1 {
				t.Fatal("wrong first crash attempt")
			}
			h := mustSpawn(t, spawned)
			select {
			case <-recovered:
				t.Fatal("uptime without successful probe reset budget")
			case <-time.After(30 * time.Millisecond):
			}
			h.crash()
			if got := recvInt(t, crashes); got != 2 {
				t.Fatalf("crash attempt after unproven uptime = %d, want 2", got)
			}
		})
	}
}

func TestHealthyResetAfterLateProbeSuccess(t *testing.T) {
	spawned := make(chan *fakeHandle, 4)
	sup := newSupervisor("test", fastPolicy(5, 10*time.Millisecond), func() (supervisedHandle, error) {
		h := newFakeHandle()
		spawned <- h
		return h, nil
	})
	probe, calls := controlledProbe()
	sup.hang = hangPolicy{probe: probe, interval: time.Millisecond, deadline: time.Second, degradeAfter: 2}
	states := captureStates(sup)
	recovered := make(chan struct{}, 1)
	sup.onRecovered = func() { recovered <- struct{}{} }
	if err := sup.Start(); err != nil {
		t.Fatal(err)
	}
	defer sup.Stop()
	receiveStatus(t, states, "running", 0)
	mustSpawn(t, spawned).crash()
	receiveStatus(t, states, "restarting", 1)
	mustSpawn(t, spawned)
	receiveStatus(t, states, "running", 1)
	select {
	case <-recovered:
		t.Fatal("reset before a successful probe")
	case <-time.After(30 * time.Millisecond):
	}
	replyProbe(t, calls, nil)
	got := receiveStatus(t, states, "running", 0)
	if got.LastError != "" {
		t.Fatalf("healthy reset did not clear error: %+v", got)
	}
	select {
	case <-recovered:
	case <-time.After(time.Second):
		t.Fatal("late successful probe did not complete healthy reset")
	}
}

func TestHealthyResetClearsCrashAfterRetryFromExhausted(t *testing.T) {
	spawned := make(chan *fakeHandle, 4)
	sup := newSupervisor("test", fastPolicy(0, 10*time.Millisecond), func() (supervisedHandle, error) {
		h := newFakeHandle()
		spawned <- h
		return h, nil
	})
	probe, calls := controlledProbe()
	sup.hang = hangPolicy{probe: probe, interval: time.Millisecond, deadline: time.Second}
	states := captureStates(sup)
	recovered := make(chan struct{}, 1)
	sup.onRecovered = func() { recovered <- struct{}{} }
	if err := sup.Start(); err != nil {
		t.Fatal(err)
	}
	defer sup.Stop()
	receiveStatus(t, states, "running", 0)
	mustSpawn(t, spawned).crash()
	receiveStatus(t, states, "exhausted", 1)
	sup.Retry()
	mustSpawn(t, spawned)
	receiveStatus(t, states, "running", 0)
	select {
	case <-recovered:
		t.Fatal("explicit Retry cleared crash error without health proof")
	case <-time.After(30 * time.Millisecond):
	}
	replyProbe(t, calls, nil)
	select {
	case <-recovered:
	case <-time.After(time.Second):
		t.Fatal("healthy explicit retry never cleared the prior crash error")
	}
}

func TestHealthyResetIgnoresPreviousHandleProbe(t *testing.T) {
	spawned := make(chan *fakeHandle, 4)
	sup := newSupervisor("test", fastPolicy(5, 10*time.Millisecond), func() (supervisedHandle, error) {
		h := newFakeHandle()
		spawned <- h
		return h, nil
	})
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseProbe := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseProbe()
	probe, calls := controlledProbe()
	var count atomic.Int32
	sup.hang = hangPolicy{probe: func(ctx context.Context) error {
		if count.Add(1) == 1 {
			close(entered)
			// Deliberately return late to exercise generation fencing even
			// when a caller fails to honour the cancellation contract.
			<-release
			return nil
		}
		return probe(ctx)
	}, interval: time.Millisecond, deadline: time.Second}
	states := captureStates(sup)
	recovered := make(chan struct{}, 4)
	sup.onRecovered = func() { recovered <- struct{}{} }
	if err := sup.Start(); err != nil {
		t.Fatal(err)
	}
	defer sup.Stop()
	receiveStatus(t, states, "running", 0)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no first-generation probe")
	}
	mustSpawn(t, spawned).crash()
	receiveStatus(t, states, "restarting", 1)
	mustSpawn(t, spawned)
	receiveStatus(t, states, "running", 1)
	// A late success from the old probe cannot supply proof for this spawn.
	// Only an actual success from this generation may reset the budget.
	releaseProbe()
	select {
	case <-recovered:
		t.Fatal("previous generation supplied health proof")
	case <-time.After(30 * time.Millisecond):
	}
	replyProbe(t, calls, nil)
	receiveStatus(t, states, "running", 0)
}

func TestSupervisorProbeDeadlineAndStop(t *testing.T) {
	h := newFakeHandle()
	entered := make(chan struct{}, 1)
	cancelled := make(chan struct{}, 1)
	sup := newSupervisor("test", fastPolicy(5, 0), func() (supervisedHandle, error) { return h, nil })
	sup.hang = hangPolicy{probe: func(ctx context.Context) error {
		entered <- struct{}{}
		<-ctx.Done()
		cancelled <- struct{}{}
		return ctx.Err()
	}, interval: time.Millisecond, deadline: 20 * time.Millisecond, degradeAfter: 1}
	states := captureStates(sup)
	if err := sup.Start(); err != nil {
		t.Fatal(err)
	}
	defer sup.Stop()
	receiveStatus(t, states, "running", 0)
	receiveStatus(t, states, "degraded", 0)
	// Discard the first (timed-out) call and observe the next in-flight call.
	<-entered
	<-cancelled
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no next probe")
	}
	done := make(chan struct{})
	go func() { sup.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Stop blocked on in-flight probe beyond its deadline")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel probe")
	}
	select {
	case <-h.stoppedC:
	default:
		t.Fatal("Stop did not tear down handle")
	}
}

func TestSupervisorNoProbesDuringBackoff(t *testing.T) {
	h := newFakeHandle()
	policy := fastPolicy(5, 0)
	policy.baseDelay, policy.maxDelay = time.Hour, time.Hour
	sup := newSupervisor("test", policy, func() (supervisedHandle, error) { return h, nil })
	var calls atomic.Int32
	sup.hang = hangPolicy{probe: func(context.Context) error { calls.Add(1); return nil }, interval: time.Millisecond, deadline: time.Second}
	states := captureStates(sup)
	if err := sup.Start(); err != nil {
		t.Fatal(err)
	}
	receiveStatus(t, states, "running", 0)
	h.crash()
	receiveStatus(t, states, "restarting", 1)
	before := calls.Load()
	select {
	case got := <-states:
		t.Fatalf("unexpected backoff state: %+v", got)
	case <-time.After(10 * time.Millisecond):
	}
	sup.Stop()
	if got := calls.Load(); got != before {
		t.Fatalf("probed without live handle: %d -> %d", before, got)
	}
}

// A failed requested spawn is still part of the existing unhealthy streak;
// merely clicking Retry cannot erase previous failures unless a spawn succeeds.
func TestSupervisorFailedRequestedSpawnSharesBudget(t *testing.T) {
	spawned := make(chan *fakeHandle, 4)
	var calls atomic.Int32
	sup := newSupervisor("test", fastPolicy(1, 0), func() (supervisedHandle, error) {
		if calls.Add(1) > 2 {
			return nil, errors.New("requested spawn failed")
		}
		h := newFakeHandle()
		spawned <- h
		return h, nil
	})
	states := captureStates(sup)
	if err := sup.Start(); err != nil {
		t.Fatal(err)
	}
	defer sup.Stop()
	receiveStatus(t, states, "running", 0)
	mustSpawn(t, spawned).crash()
	receiveStatus(t, states, "restarting", 1)
	mustSpawn(t, spawned)
	receiveStatus(t, states, "running", 1)
	sup.Retry()
	receiveStatus(t, states, "restarting", 1)
	receiveStatus(t, states, "exhausted", 2)
	if got := calls.Load(); got != 3 {
		t.Fatalf("requested failure got a fresh budget: %d spawns, want 3", got)
	}
}

func TestRetryRunningOrDegradedDoesNotCountCrash(t *testing.T) {
	for _, degraded := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "degraded"}[degraded], func(t *testing.T) {
			spawned := make(chan *fakeHandle, 4)
			sup := newSupervisor("test", fastPolicy(5, 0), func() (supervisedHandle, error) {
				h := newFakeHandle()
				spawned <- h
				return h, nil
			})
			if degraded {
				sup.hang = hangPolicy{probe: func(context.Context) error { return errors.New("wedged") }, interval: time.Millisecond, deadline: time.Second, degradeAfter: 2}
			}
			states := captureStates(sup)
			crashes := make(chan int, 4)
			sup.onCrash = func(attempt int) { crashes <- attempt }
			if err := sup.Start(); err != nil {
				t.Fatal(err)
			}
			defer sup.Stop()
			old := mustSpawn(t, spawned)
			receiveStatus(t, states, "running", 0)
			if degraded {
				receiveStatus(t, states, "degraded", 0)
			}
			sup.Retry()
			receiveStatus(t, states, "restarting", 0)
			mustSpawn(t, spawned)
			receiveStatus(t, states, "running", 0)
			select {
			case <-old.stoppedC:
			default:
				t.Fatal("Retry did not stop old handle")
			}
			select {
			case got := <-crashes:
				t.Fatalf("requested restart counted as crash %d", got)
			default:
			}
		})
	}
}
