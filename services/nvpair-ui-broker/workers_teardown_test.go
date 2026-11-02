// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type delayedStopHandle struct {
	done          chan struct{}
	once          sync.Once
	delay         time.Duration
	before, after func()
}

func (h *delayedStopHandle) Done() <-chan struct{} { return h.done }
func (h *delayedStopHandle) Stop() {
	h.once.Do(func() {
		if h.before != nil {
			h.before()
		}
		time.Sleep(h.delay)
		if h.after != nil {
			h.after()
		}
		close(h.done)
	})
}

func addTeardownWorker(t *testing.T, b *Broker, name string, h *delayedStopHandle) *supervisor {
	t.Helper()
	sup := newSupervisor(name, noRestartPolicy(), func() (supervisedHandle, error) { return h, nil })
	if err := sup.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sup.Stop)
	b.workerEntries = append(b.workerEntries, &workerEntry{sup: sup, started: true})
	return sup
}

func TestStopWorkersJoinsConcurrently(t *testing.T) {
	resetTeardownClock()
	t.Cleanup(resetTeardownClock)
	b := &Broker{}
	const delay = 200 * time.Millisecond
	for i := range 12 {
		addTeardownWorker(t, b, fmt.Sprintf("worker-%d", i), &delayedStopHandle{done: make(chan struct{}), delay: delay})
	}
	start := time.Now()
	b.stopWorkers()
	elapsed := time.Since(start)
	if elapsed < delay || elapsed > time.Second {
		t.Fatalf("worker joins took %v, want one %v exit rather than twelve sequential exits", elapsed, delay)
	}
	for _, entry := range b.workerEntries {
		select {
		case <-entry.sup.doneCh:
		default:
			t.Fatal("returned before joining every worker")
		}
	}
}

func TestStopWorkersClosesAllIngressBeforeOtherWorkers(t *testing.T) {
	resetTeardownClock()
	t.Cleanup(resetTeardownClock)
	b := &Broker{}
	var ingressClosed atomic.Int32
	var earlyStop atomic.Bool
	for _, name := range []string{"proxy", "lmstudio-proxy", "omlx-proxy"} {
		h := &delayedStopHandle{done: make(chan struct{}), delay: 200 * time.Millisecond, after: func() { ingressClosed.Add(1) }}
		sup := addTeardownWorker(t, b, name, h)
		switch name {
		case "proxy":
			b.proxySup = sup
		case "lmstudio-proxy":
			b.lmstudioProxySup = sup
		case "omlx-proxy":
			b.omlxProxySup = sup
		}
	}
	addTeardownWorker(t, b, engineManagerWorkerName, &delayedStopHandle{done: make(chan struct{}), before: func() {
		if ingressClosed.Load() != 3 {
			earlyStop.Store(true)
		}
	}})
	start := time.Now()
	b.stopWorkers() // also covers early Serve returns before shutdownInferenceStack
	if earlyStop.Load() {
		t.Fatal("engine-manager stopped before all ingress closed")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("proxy joins serialized: %v", elapsed)
	}
}

func TestStopWorkersSkipsUnstartedTree(t *testing.T) {
	resetTeardownClock()
	t.Cleanup(resetTeardownClock)
	b := &Broker{workerEntries: []*workerEntry{{sup: newSupervisor("missing", noRestartPolicy(), nil)}}}
	b.stopWorkers()
	if _, active := teardownRemaining(); active {
		t.Fatal("unstarted tree armed teardown")
	}
}

func TestTeardownWaitsDoNotExtendSpentBudget(t *testing.T) {
	setTeardownStart(time.Now().Add(-2 * teardownBudget))
	t.Cleanup(resetTeardownClock)
	if got := graceFor("scanner"); got != 0 {
		t.Errorf("spent worker grace = %v", got)
	}
	if got := engineShutdownDeadline(); got != 0 {
		t.Errorf("spent engine deadline = %v", got)
	}
	if got := teardownWait(workerSignalGrace); got != 0 {
		t.Errorf("spent escalation wait = %v", got)
	}
}

// The helper never reads stdin and ignores terminate, so only kill can reap it.
func TestTeardownHangProcess(t *testing.T) {
	if os.Getenv("PAIR_BROKER_TEST_HANG") != "1" {
		return
	}
	signal.Ignore(syscall.SIGTERM)
	fmt.Println("ready")
	time.Sleep(time.Hour)
	os.Exit(0)
}

func TestTeardownEscalationDeadlineWithSlowProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("terminate-ignoring subprocess requires Unix signals")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestTeardownHangProcess$")
	cmd.Env = append(os.Environ(), "PAIR_BROKER_TEST_HANG=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
		resetTeardownClock()
	})
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("helper readiness = %q, %v", line, err)
	}
	setTeardownStart(time.Now().Add(-(teardownBudget - 200*time.Millisecond)))
	start := time.Now()
	waitForStdinClose("scanner", cmd, stdin, done)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("escalation extended the shared deadline: %v", elapsed)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("killed worker was not reaped")
	}
}

func TestTeardownEscalationUsesOnlyRemainingBudget(t *testing.T) {
	setTeardownStart(time.Now().Add(-(teardownBudget - 100*time.Millisecond)))
	t.Cleanup(resetTeardownClock)
	if got := teardownWait(workerSignalGrace); got <= 0 || got > 100*time.Millisecond {
		t.Fatalf("escalation wait = %v, want at most the remaining 100ms", got)
	}
	resetTeardownClock()
	if got := teardownWait(workerSignalGrace); got != workerSignalGrace {
		t.Fatalf("runtime escalation wait = %v, want %v", got, workerSignalGrace)
	}
	if teardownBudget+workloadHistoryFlushJoinTimeout > 15*time.Second {
		t.Fatal("teardown and history-flush budgets exceed the desktop/TUI allowance")
	}
}
