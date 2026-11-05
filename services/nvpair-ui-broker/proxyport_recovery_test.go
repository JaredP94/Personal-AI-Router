// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	settings "nvpair-shared/enginesettings"
)

// Use the actual worker: SetPort must relay prepare-start back through the
// broker while reconciliation waits for its response, not answer inline.
func recoveryWorker(t *testing.T, intercept func(string, json.RawMessage) bool) (*settingsHarness, *rpcWorker) {
	t.Helper()
	// This is a fresh broker lifetime, independent of earlier Serve tests.
	// Do not let their process-global shutdown budget expire during real RPCs.
	resetTeardownClock()
	t.Cleanup(resetTeardownClock)
	binary := filepath.Join(t.TempDir(), "engine-manager")
	build := exec.Command("go", "build", "-race", "-o", binary, ".")
	build.Dir = "../nvpair-engine-manager"
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build worker: %v\n%s", err, output)
	}
	home, config := t.TempDir(), t.TempDir()
	for key, value := range map[string]string{"HOME": home, "APPDATA": config, "LOCALAPPDATA": config, "XDG_CONFIG_HOME": config} {
		t.Setenv(key, value)
	}
	h := newSettingsHarness(t)
	b := h.b
	b.ollamaPortReady = make(chan struct{})
	b.managedOllamaFacade.Store(true)
	b.managedOllamaBackend.Store(11436)
	b.ollamaBackendPort.Store(11435)
	worker, err := startRPCWorker("engine-manager", binary, []string{"--loaded-poll-interval=0"}, func(method string, raw json.RawMessage) {
		if intercept != nil && intercept(method, raw) {
			return
		}
		if method == "engine:settings-request" || method == "engine:settings-cancel" {
			b.forwardEngineNotification(method, raw)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	b.setEngineMgr(worker)
	t.Cleanup(func() { _ = worker.stdin.Close(); _ = worker.cmd.Process.Kill(); <-worker.done })
	return h, worker
}

func workerServerPort(t *testing.T, worker *rpcWorker) int {
	t.Helper()
	raw, rpcErr, err := worker.Call(context.Background(), "engine:get-launch", settingsJSON(map[string]string{"engine": "ollama"}))
	var launch settings.LaunchState
	if err != nil || rpcErr != nil || json.Unmarshal(raw, &launch) != nil {
		t.Fatalf("worker launch: %s, %v, %v", raw, rpcErr, err)
	}
	return launch.ServerPort
}

func waitPortReconcile(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(12 * time.Second):
		t.Fatal("managed reconciliation did not finish")
	}
}

func TestUnconfirmedManagedPortCancellationKeepsGateClosed(t *testing.T) {
	h := newSettingsHarness(t)
	b := h.b
	b.ollamaPortReady = make(chan struct{})
	b.managedOllamaFacade.Store(true)
	b.managedOllamaBackend.Store(11436)
	worker, _ := newTestRPCWorkerPipe(t)
	worker.peer.Close()
	b.setEngineMgr(worker)
	b.reconcileProxyPortOnReady(11434)
	b.reconcileProxyPortOnReady(11434) // a duplicate ready must not release it
	select {
	case <-b.ollamaPortReady:
		t.Fatal("unconfirmed cancellation released ownership gate")
	default:
	}
}

func TestManagedOllamaRecoveryRelay(t *testing.T) {
	prepared := make(chan struct{}, 1)
	h, worker := recoveryWorker(t, func(method string, _ json.RawMessage) bool {
		if method == "engine:settings-request" {
			select {
			case prepared <- struct{}{}:
			default:
			}
		}
		return false
	})
	b := h.b
	done := make(chan struct{})
	started := time.Now()
	go func() { defer close(done); b.reconcileProxyPortOnReady(11434) }()
	select {
	case <-prepared:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not request reverse recovery")
	}
	waitPortReconcile(t, done)
	elapsed := time.Since(started)
	t.Logf("reverse recovery completed in %v (excluding worker build)", elapsed)
	if elapsed > 3*time.Second {
		t.Errorf("reverse recovery hit the worker timeout: %v", elapsed)
	}
	if !b.managedOllamaFacade.Load() || b.ollamaBackendPort.Load() != 11436 {
		t.Fatalf("managed move took fallback: managed=%v backend=%d", b.managedOllamaFacade.Load(), b.ollamaBackendPort.Load())
	}
	select {
	case <-b.ollamaPortReady:
	default:
		t.Fatal("successful move left ownership gate closed")
	}
	if port := workerServerPort(t, worker); port != 11436 {
		t.Fatalf("worker port = %d", port)
	}
}

func holdFirstPrepare() (func(string, json.RawMessage) bool, <-chan json.RawMessage) {
	held := make(chan json.RawMessage, 1)
	var seen atomic.Bool
	return func(method string, raw json.RawMessage) bool {
		var relay settings.Relay
		if method == "engine:settings-request" && json.Unmarshal(raw, &relay) == nil && relay.Method == "prepare-start" && seen.CompareAndSwap(false, true) {
			held <- raw
			return true
		}
		return false
	}, held
}

func TestManagedOllamaTimeoutFencesLatePortMove(t *testing.T) {
	intercept, held := holdFirstPrepare()
	h, worker := recoveryWorker(t, intercept)
	done := make(chan struct{})
	go func() { defer close(done); h.b.reconcileProxyPortOnReady(11434) }()
	var delayed json.RawMessage
	select {
	case delayed = <-held:
	case <-time.After(3 * time.Second):
		t.Fatal("no prepare-start")
	}
	// The actual five-second worker deadline must expire, cancellation must be
	// joined, and rollback must complete through the same reverse recovery relay.
	waitPortReconcile(t, done)
	if h.b.managedOllamaFacade.Load() {
		t.Fatal("timed-out move did not take safe fallback")
	}
	if port := workerServerPort(t, worker); port != 11435 {
		t.Fatalf("rollback port = %d", port)
	}
	h.b.forwardEngineNotification("engine:settings-request", delayed)
	// The old prepare reply can arrive after the broker has abandoned the move;
	// the cancellation join guarantees it cannot persist 11436 afterwards.
	if port := workerServerPort(t, worker); port != 11435 {
		t.Fatalf("late move overwrote rollback: %d", port)
	}
	select {
	case <-h.b.ollamaPortReady:
	default:
		t.Fatal("fenced fallback did not finish")
	}
}

func TestManagedOllamaMoveRevalidatesNewSettings(t *testing.T) {
	intercept, held := holdFirstPrepare()
	h, worker := recoveryWorker(t, intercept)
	done := make(chan struct{})
	go func() { defer close(done); h.b.reconcileProxyPortOnReady(11434) }()
	var delayed json.RawMessage
	select {
	case delayed = <-held:
	case <-time.After(3 * time.Second):
		t.Fatal("no prepare-start")
	}
	request := h.request(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	request.Settings.ServerPort = listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	receipt, err := h.b.applyEngineSettings(context.Background(), request, "")
	if err != nil && strings.Contains(err.Error(), "settings changed") {
		// Port operations now queue: a fenced fallback may commit its
		// observed baseline before this Apply enters. Reload, not overwrite.
		snapshot, readErr := h.b.getEngineSettings(context.Background(), request, "")
		if readErr != nil {
			t.Fatal(readErr)
		}
		request.ExpectedRevision = snapshot.Revision
		receipt, err = h.b.applyEngineSettings(context.Background(), request, "")
	}
	if err != nil || receipt.Phase != "succeeded" {
		t.Fatalf("concurrent apply: %+v, %v", receipt, err)
	}
	h.b.forwardEngineNotification("engine:settings-request", delayed)
	waitPortReconcile(t, done)
	if port := workerServerPort(t, worker); port != request.Settings.ServerPort {
		t.Fatalf("stale managed move overwrote explicit settings: port=%d want=%d", port, request.Settings.ServerPort)
	}
	if h.b.ollamaBackendPort.Load() != int32(request.Settings.ServerPort) {
		t.Fatal("stale result overwrote authoritative broker cache")
	}
	if h.b.managedOllamaFacade.Load() {
		t.Fatal("stale result re-enabled managed ownership")
	}
}
