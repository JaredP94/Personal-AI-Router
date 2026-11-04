// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	settings "nvpair-shared/enginesettings"
	"nvpair-shared/noderec"
	"nvpair-ui-broker/relay"
)

func TestEngineStartGateRelay(t *testing.T) {
	h := newSettingsHarness(t)
	engine, replies := newTestRPCWorkerPipe(t)
	h.b.setEngineMgr(engine)
	call := func(method, token string) string {
		t.Helper()
		h.b.forwardEngineNotification("engine:settings-request", settingsJSON(settings.Relay{ID: method, Method: method, Request: settings.Request{Engine: "ollama", RequestID: token}}))
		done := make(chan *Message, 1)
		go func() { msg, _ := replies.Read(); done <- msg }()
		select {
		case msg := <-done:
			var reply struct {
				ID     string
				Result json.RawMessage
				Error  string
			}
			if msg == nil || msg.Method != "engine:settings-reply" || json.Unmarshal(msg.Params, &reply) != nil || reply.ID != method || reply.Error != "" {
				t.Fatalf("invalid start gate relay reply: %+v", msg)
			}
			var grant string
			_ = json.Unmarshal(reply.Result, &grant)
			return grant
		case <-time.After(3 * time.Second):
			t.Fatal("start gate relay deadlocked")
			return ""
		}
	}
	token := call("prepare-start", "")
	if token == "" {
		t.Fatal("recovery did not issue a grant")
	}
	h.b.settingsApplyMu.Lock()
	h.b.engineConfigMu.Lock()
	defer h.b.settingsApplyMu.Unlock()
	defer h.b.engineConfigMu.Unlock()
	call("authorize-start", token)
}

func TestStartGateRetriesRecoveryAndWithdrawsAdvertisement(t *testing.T) {
	for _, phase := range []string{"applying", "failed", "unreadable"} {
		t.Run(phase, func(t *testing.T) {
			h := newSettingsHarness(t)
			h.request(t)
			record := h.b.engineSettings["ollama"]
			record.Previous = record.Snapshot.Settings
			record.Snapshot.Phase = phase
			if phase == "unreadable" {
				path, err := h.b.engineSettingsPath()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(`{"private-launch-value":`), 0600); err != nil {
					t.Fatal(err)
				}
				h.b.engineSettingsLoaded = false
			}
			h.b.regCache = relay.NewRegistrationCache()
			h.b.regCache.Register(noderec.RegisterParams{Service: noderec.ServiceOllama, Port: 12345})
			h.fail.Store(true)
			token, err := h.b.prepareEngineStart(context.Background(), "ollama")
			if err == nil || err.Error() != settingsRecoveryError || token != "" {
				t.Fatalf("unsafe recovery grant=%q err=%v", token, err)
			}
			h.b.reconcileAdvertise(nil)
			if len(h.b.regCache.Snapshot()) != 0 || !h.b.settingsRecoveryBlocked["ollama"] {
				t.Fatal("failed recovery remained advertised")
			}
			if err := h.b.authorizeEngineStart(settings.Request{Engine: "ollama"}); err == nil {
				t.Fatal("missing grant authorized")
			}
			h.fail.Store(false)
			if phase == "unreadable" {
				path, _ := h.b.engineSettingsPath()
				if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			token, err = h.b.prepareEngineStart(context.Background(), "ollama")
			if err != nil || token == "" || h.b.settingsRecoveryBlocked["ollama"] {
				t.Fatalf("recovery retry grant=%q err=%v", token, err)
			}
			if err := h.b.authorizeEngineStart(settings.Request{Engine: "ollama", RequestID: token}); err != nil {
				t.Fatal(err)
			}
			if err := h.b.authorizeEngineStart(settings.Request{Engine: "lmstudio", RequestID: token}); err == nil {
				t.Fatal("grant crossed engine boundary")
			}
		})
	}
}

func TestConfigureRevokesStartGrantAndAuthorizesRollbackWithoutSettingsLocks(t *testing.T) {
	h := newSettingsHarness(t)
	request := h.request(t)
	token, err := h.b.prepareEngineStart(context.Background(), "ollama")
	if err != nil {
		t.Fatal(err)
	}
	h.entered = make(chan struct{}, 1)
	h.release = make(chan struct{})
	request.Settings.LaunchText += " --changed"
	done := make(chan struct{})
	go func() { defer close(done); _, _ = h.b.applyEngineSettings(context.Background(), request, "") }()
	select {
	case <-h.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("configure not entered")
	}
	defer func() { close(h.release); <-done }()
	if err := h.b.authorizeEngineStart(settings.Request{Engine: "ollama", RequestID: token}); err == nil {
		t.Fatal("accepted configure left a stale start grant valid")
	}
	h.b.settingsRelayMu.Lock()
	var operationID string
	for id, op := range h.b.activeSettings {
		if op.Engine == "ollama" {
			operationID = id
		}
	}
	h.b.settingsRelayMu.Unlock()
	if operationID == "" {
		t.Fatal("no active operation")
	}
	// These are precisely the locks held across apply/rollback. Authorization
	// at doStart must not try to reacquire either or invoke worker recovery.
	h.b.engineConfigMu.Lock()
	result := make(chan error, 1)
	go func() { result <- h.b.authorizeEngineStart(settings.Request{Engine: "ollama", RequestID: operationID}) }()
	select {
	case err := <-result:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(time.Second):
		t.Error("rollback authorization deadlocked")
	}
	h.b.engineConfigMu.Unlock()
	if err := h.b.authorizeEngineStart(settings.Request{Engine: "lmstudio", RequestID: operationID}); err == nil {
		t.Fatal("operation authorized another engine")
	}
}
