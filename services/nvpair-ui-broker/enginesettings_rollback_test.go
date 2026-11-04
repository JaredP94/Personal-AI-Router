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

func TestSettingsRollbackRelayRestoresOnlyCapturedProxyPort(t *testing.T) {
	h := newSettingsHarness(t)
	worker, replies := newTestRPCWorkerPipe(t)
	h.b.setEngineMgr(worker)
	_, previous := h.b.getOMLXProxy().Status()
	_, candidate := h.b.getLMStudioProxy().Status()
	if err := h.b.rebindSettingsProxy(context.Background(), "omlx", candidate); err != nil {
		t.Fatal(err)
	}
	h.b.activeSettings = map[string]activeSettingsOperation{"operation": {Engine: "omlx", Port: candidate, PreviousPort: previous}}
	for _, tc := range []struct {
		name, token, engine string
		port                int
		rejected            bool
	}{
		{"unknown operation", "unknown", "omlx", candidate, true},
		{"wrong engine", "operation", "ollama", candidate, true},
		{"nominated rollback port", "operation", "omlx", previous, true},
		{"captured previous port", "operation", "omlx", candidate, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.b.handleEngineSettingsRead(settingsJSON(settings.Relay{ID: tc.name, Method: "rollback", Request: settings.Request{Engine: tc.engine, RequestID: tc.token, Settings: settings.Config{ProxyPort: tc.port}}}))
			done := make(chan bool, 1)
			go func() {
				msg, err := replies.Read()
				var reply struct{ ID, Error string }
				if err != nil || json.Unmarshal(msg.Params, &reply) != nil || reply.ID != tc.name {
					done <- false
					return
				}
				done <- (reply.Error != "") == tc.rejected
			}()
			select {
			case ok := <-done:
				if !ok {
					t.Fatal("incorrect rollback relay outcome")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("rollback relay did not complete")
			}
			want := candidate
			if !tc.rejected {
				want = previous
			}
			ready, port := h.b.getOMLXProxy().Status()
			if !ready || port != want {
				t.Fatal("rollback changed an unauthorized port or failed to restore the captured binding")
			}
			if !tc.rejected && int(h.b.omlxProxyStartupPort.Load()) != previous {
				t.Fatal("startup would replay the failed candidate port")
			}
		})
	}
}

func TestSettingsFailedRecoveryCannotReplayCandidateOrChangeReceipt(t *testing.T) {
	h := newSettingsHarness(t)
	request := h.request(t)
	previous := request.Settings
	request.Settings.LaunchText += " --invalid"
	h.fail.Store(true)
	receipt, err := h.b.applyEngineSettings(context.Background(), request, "")
	if err != nil || receipt.Phase != "failed" {
		t.Fatal("expected a failed operation")
	}
	if len(h.b.recoverEngineSettings()) == 0 {
		t.Fatal("failed compensation allowed automatic engine restoration")
	}
	h.fail.Store(false)
	if len(h.b.recoverEngineSettings()) != 0 {
		t.Fatal("previous configuration could not be reconciled")
	}
	if h.applies.Load() != 3 {
		t.Fatal("failed component stores were not reconciled before automatic startup")
	}
	record := h.b.engineSettings["ollama"]
	if record.Snapshot.Settings != previous || record.Snapshot.Phase != "failed" || record.Receipts[request.RequestID] != receipt {
		t.Fatal("recovery replayed candidate settings or rewrote the failed receipt")
	}
}

func TestSettingsFailedRevisionPersistsRestoredConfiguration(t *testing.T) {
	h := newSettingsHarness(t)
	request := h.request(t)
	previous := request.Settings
	request.Settings.LaunchText += " --invalid"
	h.fail.Store(true)
	receipt, err := h.b.applyEngineSettings(context.Background(), request, "")
	if err != nil || receipt.Phase != "failed" {
		t.Fatal("failed apply did not produce a failed receipt")
	}
	duplicate, err := h.b.applyEngineSettings(context.Background(), request, "")
	if err != nil || duplicate != receipt || h.applies.Load() != 1 {
		t.Fatal("retry repeated the failed operation")
	}
	h.b.engineSettingsLoaded = false
	if err := h.b.loadEngineSettingsLocked(); err != nil {
		t.Fatal(err)
	}
	snapshot := h.b.engineSettings["ollama"].Snapshot
	if snapshot.Settings != previous || !snapshot.Running || snapshot.Phase != "failed" || snapshot.Error == "" {
		t.Fatal("journal did not retain restoration outcome and previous configuration")
	}
	if config, ok := h.b.explicitEngineSettingsLocked("ollama"); !ok || config != previous {
		t.Fatal("startup would replay the failed launch settings")
	}
}
