// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	settings "nvpair-shared/enginesettings"
)

func TestPrepareStartCompletesWhilePortOperationHeld(t *testing.T) {
	for _, phase := range []string{"idle", "failed", "applying"} {
		t.Run(phase, func(t *testing.T) {
			h := newSettingsHarness(t)
			h.request(t)
			h.b.engineConfigMu.Lock()
			h.b.engineSettings["ollama"].Snapshot.Phase = phase
			h.b.engineConfigMu.Unlock()
			worker, replies := newTestRPCWorkerPipe(t)
			h.b.setEngineMgr(worker)
			finish := h.b.beginPortOperation("ollama")
			defer finish()
			h.b.forwardEngineNotification("engine:settings-request", settingsJSON(settings.Relay{
				ID: "prepare-held", Method: "prepare-start", Request: settings.Request{Engine: "ollama"},
			}))
			done := make(chan *Message, 1)
			go func() { reply, _ := replies.Read(); done <- reply }()
			select {
			case reply := <-done:
				var result struct {
					Result string
					Error  string
				}
				if reply == nil || reply.Method != "engine:settings-reply" || json.Unmarshal(reply.Params, &result) != nil {
					t.Fatalf("invalid reverse callback reply: %+v", reply)
				}
				if phase == "idle" && (result.Error != "" || result.Result == "") {
					t.Fatalf("healthy preparation rejected: %+v", result)
				}
				if phase != "idle" && (result.Error != settingsRecoveryError || result.Result != "") {
					t.Fatalf("unreconciled preparation was not fail-closed: %+v", result)
				}
			case <-time.After(time.Second):
				t.Fatal("prepare-start waited for the parent port operation")
			}
		})
	}
}

func TestPortOperationConvergenceFailureBlocksStart(t *testing.T) {
	h := newSettingsHarness(t)
	request := h.request(t)
	token, err := h.b.prepareEngineStart(context.Background(), "ollama", "")
	if err != nil {
		t.Fatal(err)
	}
	h.b.engineConfigMu.Lock()
	h.b.engineSettings["ollama"].Explicit = true
	h.b.engineConfigMu.Unlock()
	worker, _ := newTestRPCWorkerPipe(t)
	worker.peer.Close()
	h.b.setProxy(&proxyProcess{peer: worker.peer, ready: true, port: request.Settings.ProxyPort + 1})
	finish := h.b.beginPortOperation("ollama")
	finish()
	h.b.engineConfigMu.Lock()
	blocked := h.b.settingsRecoveryBlocked["ollama"]
	h.b.engineConfigMu.Unlock()
	if !blocked {
		t.Fatal("failed final bind did not gate availability")
	}
	if err := h.b.authorizeEngineStart(settings.Request{Engine: "ollama", RequestID: token}); err == nil {
		t.Fatal("failed convergence retained an old start grant")
	}
	if token, err := h.b.prepareEngineStart(context.Background(), "ollama", ""); err == nil || token != "" {
		t.Fatalf("failed final convergence issued a start grant: %q %v", token, err)
	}
}

func TestPortOperationCorrectsStaleReadyToAcceptedPort(t *testing.T) {
	for _, engine := range []string{"ollama", "lmstudio", "omlx"} {
		t.Run(engine, func(t *testing.T) {
			h := newSettingsHarness(t)
			request := h.request(t)
			request.Engine = engine
			snapshot, err := h.b.getEngineSettings(context.Background(), request, "")
			if err != nil {
				t.Fatal(err)
			}
			request.Settings = snapshot.Settings
			request.ExpectedRevision = snapshot.Revision
			request.Settings.LaunchText += " --accepted"
			receipt, err := h.b.applyEngineSettings(context.Background(), request, "")
			if err != nil || receipt.Phase != "succeeded" {
				t.Fatalf("apply: %+v %v", receipt, err)
			}
			proxy := h.b.settingsProxy(engine)
			proxy.readyMu.Lock()
			proxy.port = 11434 // a stale persisted bind, not accepted intent
			proxy.readyMu.Unlock()
			switch engine {
			case "ollama":
				h.b.reconcileProxyPortOnReady(11434)
			case "lmstudio":
				h.b.reconcileLMStudioProxyPortOnReady(11434)
			case "omlx":
				h.b.reconcileOMLXProxyPortOnReadyForGeneration(0, 11434)
			}
			_, actual := proxy.Status()
			if actual != request.Settings.ProxyPort {
				t.Fatalf("proxy port %d, accepted %d", actual, request.Settings.ProxyPort)
			}
			snapshot, err = h.b.getEngineSettings(context.Background(), request, "")
			if err != nil || snapshot.Settings != request.Settings {
				t.Fatalf("stale bind changed desired settings: %+v %v", snapshot, err)
			}
			path, _ := h.b.engineSettingsPath()
			data, err := os.ReadFile(path)
			var records map[string]*engineSettingsRecord
			if err != nil || json.Unmarshal(data, &records) != nil || records[engine].Snapshot.Settings != request.Settings {
				t.Fatalf("persisted choices changed: %s %v", data, err)
			}
		})
	}
}

// On the old implementation Apply can finish before the suspended old write.
// A serialised implementation must keep Apply queued until that write joins.
func TestPortOperationSerializesDelayedReconcileWithApply(t *testing.T) {
	h := newSettingsHarness(t)
	request := h.request(t)
	worker, codec := newTestRPCWorkerPipe(t)
	_, initial := h.b.getProxy().Status()
	proxy := &proxyProcess{peer: worker.peer, ready: true, port: initial}
	h.b.setProxy(proxy)
	h.b.managedOllamaFacade.Store(true)
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	go func() {
		first := true
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			if msg.Method != "set-port" {
				_ = codec.Respond(msg.ID, nil)
				continue
			}
			delay := first
			first = false
			go func() {
				if delay {
					close(entered)
					<-release
				}
				var p struct{ Port int }
				_ = json.Unmarshal(msg.Params, &p)
				proxy.readyMu.Lock()
				proxy.port = p.Port
				proxy.readyMu.Unlock()
				_ = codec.Respond(msg.ID, map[string]int{"port": p.Port})
			}()
		}
	}()
	reconciled := make(chan struct{})
	go func() { defer close(reconciled); h.b.reconcileProxyPortOnReady(initial) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("old proxy write not entered")
	}
	request.Settings.LaunchText += " --newer"
	applied := make(chan error, 1)
	go func() {
		receipt, err := h.b.applyEngineSettings(context.Background(), request, "")
		if err != nil && strings.Contains(err.Error(), "settings changed") {
			// Serialisation can make the formerly concurrent managed bind
			// visible first. Reload its revision, preserving the user choices.
			snapshot, readErr := h.b.getEngineSettings(context.Background(), request, "")
			if readErr == nil {
				request.ExpectedRevision = snapshot.Revision
				receipt, err = h.b.applyEngineSettings(context.Background(), request, "")
			}
		}
		if err == nil && receipt.Phase != "succeeded" {
			t.Errorf("apply receipt: %+v", receipt)
		}
		applied <- err
	}()
	select {
	case err := <-applied:
		if err != nil {
			t.Fatal(err)
		}
		// e45aebf reaches here: only now allow the stale bind to persist.
		close(release)
	case <-time.After(100 * time.Millisecond):
		close(release)
		select {
		case err := <-applied:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("queued apply did not finish")
		}
	}
	waitPortReconcile(t, reconciled)
	_, got := proxy.Status()
	if got != request.Settings.ProxyPort {
		t.Fatalf("stale reconciliation persisted proxy port %d; Apply accepted %d", got, request.Settings.ProxyPort)
	}
	path, _ := h.b.engineSettingsPath()
	data, err := os.ReadFile(path)
	var records map[string]*engineSettingsRecord
	if err != nil || json.Unmarshal(data, &records) != nil || records["ollama"].Snapshot.Settings != request.Settings {
		t.Fatalf("persisted settings differ from Apply: %s, %v", data, err)
	}
}
