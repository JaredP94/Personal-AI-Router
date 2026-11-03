// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// This subprocess does no engine or discovery work. It needs time after EOF to
// clean up; a successful exit proves it wasn't terminated before finishing.
func TestTeardownCleanupProcess(t *testing.T) {
	delayText := os.Getenv("PAIR_BROKER_TEST_CLEANUP_MS")
	if delayText == "" {
		return
	}
	delay, err := strconv.Atoi(delayText)
	if err != nil {
		os.Exit(2)
	}
	if os.Getenv("PAIR_BROKER_TEST_IGNORE_TERM") == "1" {
		signal.Ignore(syscall.SIGTERM)
	}
	_, _ = io.WriteString(os.Stdout, "ready\n")
	_, _ = io.Copy(io.Discard, os.Stdin)
	time.Sleep(time.Duration(delay) * time.Millisecond)
	os.Exit(0)
}

func startTeardownCleanupProcess(t *testing.T, name string, delay time.Duration, ignoreTerm bool) *rpcWorker {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestTeardownCleanupProcess$")
	cmd.Env = append(os.Environ(), "PAIR_BROKER_TEST_CLEANUP_MS="+strconv.FormatInt(delay.Milliseconds(), 10))
	if ignoreTerm {
		cmd.Env = append(cmd.Env, "PAIR_BROKER_TEST_IGNORE_TERM=1")
	}
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
	w := &rpcWorker{name: name, cmd: cmd, stdin: stdin, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(w.done) }()
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Process.Kill(); <-w.done })
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("%s readiness = %q, %v", name, line, err)
	}
	return w
}

func TestProxyTeardownReservesPreparationAndCleanup(t *testing.T) {
	for _, name := range []string{"proxy", "lmstudio-proxy", "omlx-proxy"} {
		t.Run(name, func(t *testing.T) {
			resetTeardownClock()
			t.Cleanup(resetTeardownClock)
			if got := graceFor(name); got != workerStopGrace {
				t.Fatalf("runtime proxy grace = %v, want %v", got, workerStopGrace)
			}
			if got := signalWaitFor(name); got != workerSignalGrace {
				t.Fatalf("runtime proxy escalation = %v, want %v", got, workerSignalGrace)
			}
			beginTeardown()
			if got := graceFor(name); got != proxyStopGrace {
				t.Fatalf("teardown proxy grace = %v, want %v", got, proxyStopGrace)
			}
			if got := signalWaitFor(name); got != proxySignalGrace {
				t.Fatalf("teardown proxy escalation = %v, want %v", got, proxySignalGrace)
			}
			setTeardownStart(time.Now().Add(-(teardownBudget - enginePreparationReserve - workerCleanupReserve)))
			if graceFor(name) != 0 || signalWaitFor(name) != 0 {
				t.Fatal("proxy waits consumed the later phases' reserve")
			}
			if got := engineShutdownDeadline(); got < enginePreparationReserve-100*time.Millisecond {
				t.Fatalf("engine preparation lost its reserve: %v", got)
			}
		})
	}
}

func TestTeardownSlowEarlierPhasesPreserveWorkerCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("terminate-ignoring proxy fixture requires Unix signals")
	}
	resetTeardownClock()
	t.Cleanup(resetTeardownClock)
	b := &Broker{}
	// The proxy's orderly exit would take five seconds and it ignores TERM.
	proxy := startTeardownCleanupProcess(t, "proxy", 5*time.Second, true)
	b.proxySup = newSupervisor("proxy", noRestartPolicy(), func() (supervisedHandle, error) { return proxy, nil })
	if err := b.proxySup.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.proxySup.Stop)
	b.workerEntries = append(b.workerEntries, &workerEntry{sup: b.proxySup, started: true})

	later := []*rpcWorker{
		startTeardownCleanupProcess(t, "scanner", time.Second, false),
		startTeardownCleanupProcess(t, engineManagerWorkerName, 1500*time.Millisecond, false),
	}
	for _, worker := range later {
		sup := newSupervisor(worker.name, noRestartPolicy(), func() (supervisedHandle, error) { return worker, nil })
		if err := sup.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(sup.Stop)
		b.workerEntries = append(b.workerEntries, &workerEntry{sup: sup, started: true})
	}

	client, server := net.Pipe()
	peer := NewPeer(NewCodec(client))
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	go peer.Serve(nil, nil)
	preparation := make(chan struct{})
	release := make(chan struct{})
	preparationDone := make(chan struct{})
	go func() {
		defer close(preparationDone)
		msg, err := NewCodec(server).Read()
		if err == nil && msg.Method == prepareEngineShutdownMethod {
			close(preparation)
		}
		<-release // no response: exercise the real engine preparation deadline
	}()
	t.Cleanup(func() { close(release); _ = server.Close(); <-preparationDone })
	b.setEngineMgr(&rpcWorker{peer: peer})

	start := time.Now()
	b.shutdownInferenceStack()
	select {
	case <-preparation:
	default:
		t.Error("engine preparation request was not sent")
	}
	// The final concurrent join must retain both workers' normal EOF graces.
	if got := graceFor("scanner"); got < workerStopGrace-100*time.Millisecond {
		t.Errorf("scanner cleanup grace was consumed by earlier phases: %v", got)
	}
	if got := graceFor(engineManagerWorkerName); got < engineManagerStopGrace-100*time.Millisecond {
		t.Errorf("engine-manager cleanup grace was consumed by earlier phases: %v", got)
	}
	b.stopWorkers()
	for _, worker := range later {
		select {
		case <-worker.done:
			if code := worker.cmd.ProcessState.ExitCode(); code != 0 {
				t.Errorf("%s was escalated before cleanup completed: exit %d", worker.name, code)
			}
		case <-time.After(time.Second):
			t.Errorf("%s exit was not joined", worker.name)
		}
	}
	if elapsed := time.Since(start); elapsed > teardownBudget+100*time.Millisecond {
		t.Errorf("shutdown took %v, exceeds the %v broker budget", elapsed, teardownBudget)
	}
}
