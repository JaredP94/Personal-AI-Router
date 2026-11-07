// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	settings "nvpair-shared/enginesettings"
	serviceerrors "nvpair-shared/errors"
	"nvpair-shared/noderec"
	"nvpair-ui-broker/relay"
)

func captureSettingsRecoveryErrors(t *testing.T, h *settingsHarness) <-chan *Message {
	t.Helper()
	worker, codec := newTestRPCWorkerPipe(t)
	h.b.setErrors(&errorsProcess{peer: worker.peer})
	messages := make(chan *Message, 100)
	go func() {
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			messages <- msg
		}
	}()
	return messages
}

func assertSettingsRecoveryError(t *testing.T, messages <-chan *Message, engine string) {
	t.Helper()
	for {
		select {
		case msg := <-messages:
			if msg.Method != methodErrorsReport {
				continue
			}
			var report serviceerrors.ServiceError
			if err := json.Unmarshal(msg.Params, &report); err != nil {
				t.Fatal(err)
			}
			if report.ID != "engine-settings:recovery:"+engine || report.NodeID != "target" || report.Severity != "error" || !strings.Contains(report.Message, "restart") || strings.Contains(report.Message, "candidate") || strings.Contains(report.Message, "fixture") {
				t.Fatalf("unsafe or unclear recovery error: %+v", report)
			}
			return
		case <-time.After(3 * time.Second):
			t.Fatal("recovery failure was not sent to the errors channel")
		}
	}
}

func assertSettingsRecoveryCleared(t *testing.T, messages <-chan *Message, engine string) {
	t.Helper()
	for {
		select {
		case msg := <-messages:
			var cleared serviceerrors.ClearParams
			if msg.Method == methodErrorsClear && json.Unmarshal(msg.Params, &cleared) == nil && cleared.ID == "engine-settings:recovery:"+engine {
				return
			}
		case <-time.After(3 * time.Second):
			t.Fatal("successful recovery did not clear its error")
		}
	}
}

func settingsRestoreNotification(t *testing.T, h *settingsHarness) []string {
	t.Helper()
	for {
		select {
		case msg := <-h.notifications:
			if msg.Method != restoreEnabledEnginesMethod {
				continue
			}
			var params struct {
				ExcludedEngines []string `json:"excludedEngines"`
			}
			if len(msg.Params) != 0 {
				if err := json.Unmarshal(msg.Params, &params); err != nil {
					t.Fatal(err)
				}
			}
			return params.ExcludedEngines
		case <-time.After(3 * time.Second):
			t.Fatal("startup did not send enabled-engine restoration")
		}
	}
}

func TestExplicitRestartRetriesSettingsRecoveryBeforeStarting(t *testing.T) {
	for _, method := range []string{"engine:start", "engine:restart"} {
		t.Run(method, func(t *testing.T) {
			h := newSettingsHarness(t)
			h.request(t)
			record := h.b.engineSettings["ollama"]
			record.Previous = record.Snapshot.Settings
			record.Snapshot.Phase = "applying"
			h.fail.Store(true)
			h.b.restoreEnabledEngines(h.b.getEngineMgr())
			settingsRestoreNotification(t, h)
			id := json.RawMessage(`1`)
			msg := &Message{JSONRPC: "2.0", ID: &id, Method: method, Params: json.RawMessage(`{"engine":"ollama"}`)}
			h.b.relayToEngineNow(msg)
			if h.starts.Load() != 0 || h.applies.Load() != 2 {
				t.Fatal("explicit retry started an engine before reconciliation succeeded")
			}
			h.fail.Store(false)
			h.b.relayToEngineNow(msg)
			if h.starts.Load() != 1 || h.applies.Load() != 3 || h.b.settingsRecoveryBlocked["ollama"] {
				t.Fatal("successful explicit retry did not reconcile before starting")
			}
		})
	}
}

func TestExplicitStartBeforeStartupReconcilesSettings(t *testing.T) {
	h := newSettingsHarness(t)
	h.request(t)
	record := h.b.engineSettings["ollama"]
	record.Previous = record.Snapshot.Settings
	record.Snapshot.Phase = "applying"
	h.fail.Store(true)
	id := json.RawMessage(`1`)
	h.b.relayToEngineNow(&Message{JSONRPC: "2.0", ID: &id, Method: "engine:start", Params: json.RawMessage(`{"engine":"ollama"}`)})
	if h.starts.Load() != 0 || h.applies.Load() != 1 || !h.b.settingsRecoveryBlocked["ollama"] {
		t.Fatal("start arriving before startup recovery bypassed reconciliation")
	}
}

func TestStartupWithoutManagerStillSuppressesUnreconciledEngine(t *testing.T) {
	h := newSettingsHarness(t)
	h.request(t)
	record := h.b.engineSettings["ollama"]
	record.Previous = record.Snapshot.Settings
	record.Snapshot.Phase = "applying"
	reports := captureSettingsRecoveryErrors(t, h)
	h.b.setEngineMgr(nil)
	h.b.restoreEnabledEngines(nil)
	assertSettingsRecoveryError(t, reports, "ollama")
	h.b.regCache = relay.NewRegistrationCache()
	h.b.regCache.Register(noderec.RegisterParams{Service: noderec.ServiceOllama, Port: 12345})
	h.b.reconcileAdvertise(nil)
	if len(h.b.regCache.Snapshot()) != 0 || h.applies.Load() != 0 {
		t.Fatal("missing manager allowed unreconciled engine availability")
	}
}

func TestStartupExcludesEngineWhenRecoveryResultCannotBeSaved(t *testing.T) {
	h := newSettingsHarness(t)
	h.request(t)
	record := h.b.engineSettings["ollama"]
	record.Previous = record.Snapshot.Settings
	record.Snapshot.Phase = "applying"
	h.failResultSave.Store(true)
	h.b.restoreEnabledEngines(h.b.getEngineMgr())
	if excluded := settingsRestoreNotification(t, h); !slices.Equal(excluded, []string{"ollama"}) {
		t.Fatalf("unsaved reconciliation exclusions = %v", excluded)
	}
}

func TestStartupUnreadableSettingsJournalBlocksAllManagedEngines(t *testing.T) {
	h := newSettingsHarness(t)
	path, err := h.b.engineSettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"private-launch-value":`), 0600); err != nil {
		t.Fatal(err)
	}
	h.b.restoreEnabledEngines(h.b.getEngineMgr())
	if excluded := settingsRestoreNotification(t, h); !slices.Equal(excluded, []string{"ollama", "lmstudio", "omlx", "llamacpp"}) {
		t.Fatalf("unreadable journal exclusions = %v", excluded)
	}
	for _, reconcile := range []func(){
		func() { h.b.reconcileAdvertise(nil) },
		func() { h.b.reconcileAdvertiseLMStudio(nil) },
		func() { h.b.reconcileAdvertiseOMLX(nil) },
	} {
		reconcile() // no health or status probing is allowed
	}
	// Repairing the journal and explicitly restarting retries the read too.
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	id := json.RawMessage(`1`)
	h.b.relayToEngineNow(&Message{JSONRPC: "2.0", ID: &id, Method: "engine:restart", Params: json.RawMessage(`{"engine":"ollama"}`)})
	if h.starts.Load() != 1 || h.b.settingsRecoveryBlocked["ollama"] {
		t.Fatal("explicit restart did not reread the repaired journal")
	}
}

func TestStartupExcludesUnreconciledEngineFromRestoration(t *testing.T) {
	for _, phase := range []string{"failed", "applying"} {
		t.Run(phase, func(t *testing.T) {
			h := newSettingsHarness(t)
			reports := captureSettingsRecoveryErrors(t, h)
			request := h.request(t)
			if _, err := h.b.getEngineSettings(context.Background(), settings.Request{Engine: "lmstudio"}, ""); err != nil {
				t.Fatal(err)
			}
			other := h.b.engineSettings["lmstudio"]
			other.Previous = other.Snapshot.Settings
			other.Snapshot.Phase = "applying"
			h.failOllamaOnly.Store(true)
			record := h.b.engineSettings["ollama"]
			record.Previous = request.Settings
			record.Snapshot.Phase = phase
			if phase == "applying" {
				record.Snapshot.Settings.LaunchText += " --candidate"
			}
			record.Resume = true
			if err := h.b.saveEngineSettingsLocked(); err != nil {
				t.Fatal(err)
			}
			h.fail.Store(true)
			// Drive the actual broker startup orchestration, not recovery alone.
			h.b.ollamaPortReady = make(chan struct{})
			h.b.lmstudioPortReady = make(chan struct{})
			close(h.b.ollamaPortReady)
			close(h.b.lmstudioPortReady)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if !h.b.runEngineAvailabilityAfterPortGates(ctx, func(context.Context) {}, func(context.Context) {}, func(context.Context) {}) {
				t.Fatal("startup gate did not complete")
			}
			if excluded := settingsRestoreNotification(t, h); !slices.Equal(excluded, []string{"ollama"}) {
				t.Fatalf("startup restoration exclusions = %v, want only ollama (other engines must remain eligible)", excluded)
			}
			if other.Snapshot.Phase != "succeeded" {
				t.Fatal("one engine's recovery failure prevented another engine's reconciliation")
			}
			assertSettingsRecoveryError(t, reports, "ollama")
			// A healthy-looking endpoint must not revive an unsafe advertisement.
			h.b.regCache = relay.NewRegistrationCache()
			h.b.regCache.Register(noderec.RegisterParams{Service: noderec.ServiceOllama, Port: 12345})
			h.b.reconcileAdvertise(nil) // must not reach health probing
			if len(h.b.regCache.Snapshot()) != 0 {
				t.Fatal("unreconciled engine was advertised")
			}
			// The same caller is used after engine-manager worker respawn.
			// Once reconciliation succeeds, the next startup can restore it.
			h.fail.Store(false)
			h.b.restoreEnabledEngines(h.b.getEngineMgr())
			if excluded := settingsRestoreNotification(t, h); len(excluded) != 0 {
				t.Fatalf("successful startup retry still excludes engines: %v", excluded)
			}
			assertSettingsRecoveryCleared(t, reports, "ollama")
		})
	}
}
