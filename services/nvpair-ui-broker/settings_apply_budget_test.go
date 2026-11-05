// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	settings "nvpair-shared/enginesettings"
)

func TestSettingsApplyBudgetIncludesAdmission(t *testing.T) {
	for _, held := range []string{"port-operation", "apply", "journal"} {
		t.Run(held, func(t *testing.T) {
			h := newSettingsHarness(t)
			request := h.request(t)
			mu := &h.b.settingsApplyMu
			if held == "port-operation" {
				mu = &h.b.portOperation(request.Engine).mu
			}
			if held == "journal" {
				mu = &h.b.engineConfigMu
			}
			mu.Lock()
			defer mu.Unlock()
			done := make(chan error, 1)
			go func() {
				_, err := h.b.applyEngineSettingsWithinBudget(context.Background(), request, "", settingsApplyTiming{total: 200 * time.Millisecond, execution: 100 * time.Millisecond, cleanup: 50 * time.Millisecond})
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("busy admission succeeded")
				}
			case <-time.After(200 * time.Millisecond * 14 / 12):
				t.Fatal("admission escaped desktop envelope")
			}
			if h.applies.Load() != 0 {
				t.Fatal("expired admission mutated the engine")
			}
		})
	}
}

func TestSettingsApplyBudgetIncludesConcurrentSnapshot(t *testing.T) {
	h := newSettingsHarness(t)
	request := h.request(t)
	request.Settings.LaunchText += " --concurrent-read"
	h.entered, h.release = make(chan struct{}, 1), make(chan struct{})
	defer close(h.release)
	const budget = 400 * time.Millisecond
	done := make(chan settingsReceipt, 1)
	go func() {
		receipt, _ := h.b.applyEngineSettingsWithinBudget(context.Background(), request, "", settingsApplyTiming{total: budget, execution: budget / 2, cleanup: budget / 4})
		done <- receipt
	}()
	select {
	case <-h.entered:
	case <-time.After(time.Second):
		t.Fatal("configure not entered")
	}
	// Block the fixture response to an unrelated snapshot taken while the
	// worker RPC has released engineConfigMu. It must inherit the work deadline.
	h.launchMu.Lock()
	defer h.launchMu.Unlock()
	read := make(chan error, 1)
	go func() {
		_, err := h.b.getEngineSettings(context.Background(), settings.Request{Engine: "lmstudio"}, "")
		read <- err
	}()
	select {
	case receipt := <-done:
		if receipt.Phase != "failed" {
			t.Fatalf("expired configure: %+v", receipt)
		}
	case <-time.After(budget * 14 / 12):
		t.Fatal("concurrent snapshot pushed Apply past desktop envelope")
	}
	select {
	case err := <-read:
		if err == nil {
			t.Fatal("suspended read succeeded")
		}
	default:
		t.Fatal("snapshot outlived Apply work deadline")
	}
}

func TestSettingsApplyBudgetIncludesFinalConvergence(t *testing.T) {
	h := newSettingsHarness(t)
	request := h.request(t)
	worker, codec := newTestRPCWorkerPipe(t)
	_, initial := h.b.getProxy().Status()
	proxy := &proxyProcess{peer: worker.peer, ready: true, port: initial + 1}
	h.b.setProxy(proxy)
	h.b.engineConfigMu.Lock()
	h.b.engineSettings["ollama"].Explicit = true
	h.b.engineConfigMu.Unlock()
	cancellation := make(chan struct{})
	go func() {
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			if msg.Method == "internal:cancel-set-port" {
				close(cancellation)
			}
			// Neither the convergence write nor its cancellation is acknowledged.
		}
	}()
	const budget = 400 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		_, err := h.b.applyEngineSettingsWithinBudget(context.Background(), request, "", settingsApplyTiming{total: budget, execution: budget / 2, cleanup: budget / 4})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("unconfirmed convergence reported success")
		}
	case <-time.After(budget * 14 / 12):
		t.Fatal("convergence/fence escaped the Apply budget")
	}
	select {
	case <-cancellation:
	default:
		t.Fatal("convergence was not fenced")
	}
	if ready, _ := proxy.Status(); ready {
		t.Fatal("unfenced convergence proxy remains available")
	}
	if !h.b.settingsApplyMu.TryLock() {
		t.Fatal("convergence retained Apply mutex")
	}
	h.b.settingsApplyMu.Unlock()
	// Ensure the failed transport cannot accept another port operation.
	if _, _, err := writeProxyPort(context.Background(), proxy, json.RawMessage(`{"port":11435}`)); err == nil {
		t.Fatal("retired proxy accepted a write")
	}
}

func TestSettingsApplyBudgetIncludesExecutionAndCallbackJoin(t *testing.T) {
	h := newSettingsHarness(t)
	request := h.request(t)
	request.Settings.LaunchText += " --budget"
	h.entered, h.release = make(chan struct{}, 1), make(chan struct{})
	defer close(h.release)
	const budget = 400 * time.Millisecond
	done := make(chan settingsReceipt, 1)
	started := time.Now()
	go func() {
		receipt, _ := h.b.applyEngineSettingsWithinBudget(context.Background(), request, "", settingsApplyTiming{total: budget, execution: budget / 2, cleanup: budget / 4})
		done <- receipt
	}()
	select {
	case <-h.entered:
	case <-time.After(time.Second):
		t.Fatal("configure not entered")
	}
	h.b.settingsRelayMu.Lock()
	var operation activeSettingsOperation
	for _, active := range h.b.activeSettings {
		operation = active
	}
	operation.Callbacks.Add(1)
	h.b.settingsRelayMu.Unlock()
	defer operation.Callbacks.Done()
	select {
	case receipt := <-done:
		if receipt.Phase != "failed" {
			t.Fatalf("expired operation did not fail safely: %+v", receipt)
		}
	case <-time.After(budget * 14 / 12):
		t.Fatal("execution/join restarted the Apply budget")
	}
	if time.Since(started) >= budget*14/12 {
		t.Fatal("result arrived after the outer envelope")
	}
	if operation.Context.Err() == nil {
		t.Fatal("callbacks remain authorized")
	}
	if ready, _ := operation.Proxy.Status(); ready {
		t.Fatal("unjoined callback proxy remains available")
	}
}
