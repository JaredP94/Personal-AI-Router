// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	settings "nvpair-shared/enginesettings"
)

// Unit executors have no broker process. Explicitly supply their reconciled
// authority, including the active ConfigureLaunch operations in those fixtures.
func reconciledTestExecutor(e *Executor) *Executor {
	e.settingsParent = func(_ context.Context, method string, p settings.Request, _ string) (json.RawMessage, error) {
		if _, ok := e.reg.Get(p.Engine); !ok {
			return nil, fmt.Errorf("unknown fixture engine")
		}
		switch method {
		case "prepare-start":
			return json.RawMessage(`"reconciled-test-generation"`), nil
		case "authorize-start":
			// Legacy unit ConfigureLaunch fixtures omit their operation ID;
			// only these explicitly trusted fixtures authorize that empty ID.
			if p.RequestID != "" && p.RequestID != "reconciled-test-generation" {
				return nil, fmt.Errorf("invalid fixture grant")
			}
			return json.RawMessage(`null`), nil
		default:
			return nil, fmt.Errorf("unexpected test authority call")
		}
	}
	return e
}

func allowReconciledTestStarts(t *testing.T, m *Manager) {
	t.Helper()
	m.settingsRelay.send = func(method string, params any) error {
		raw, err := json.Marshal(params)
		if err != nil {
			return err
		}
		var relay settings.Relay
		if method != "engine:settings-request" || json.Unmarshal(raw, &relay) != nil || (relay.Method != "prepare-start" && relay.Method != "authorize-start") {
			return fmt.Errorf("unexpected test relay")
		}
		reply, _ := json.Marshal(map[string]any{"id": relay.ID, "result": "reconciled-test-generation"})
		m.settingsRelay.reply(reply)
		return nil
	}
}

func TestRecoveryGateBlocksLocalAndRemoteStart(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(fmt.Sprint("remote=", remote), func(t *testing.T) {
			e := settingsExecutor(t, false)
			attempts := 0
			recoverable := false
			e.settingsParent = func(_ context.Context, method string, p settings.Request, _ string) (json.RawMessage, error) {
				if method == "prepare-start" {
					attempts++
					if !recoverable {
						return nil, fmt.Errorf("private launch value must never escape")
					}
					return json.RawMessage(`"reconciled"`), nil
				}
				if method == "authorize-start" && p.Engine == "fake" && p.RequestID == "reconciled" {
					return json.RawMessage(`null`), nil
				}
				return nil, fmt.Errorf("not authorized")
			}
			start := func() string {
				if remote {
					rec := httptest.NewRecorder()
					req := httptest.NewRequest(http.MethodPost, controlStartPath, strings.NewReader(`{"engine":"fake"}`))
					(&controlServer{exec: e}).handleStart(rec, req)
					if rec.Code == http.StatusOK {
						return ""
					}
					return rec.Body.String()
				}
				if err := e.Start(context.Background(), "fake"); err != nil {
					return err.Error()
				}
				return ""
			}
			message := start()
			if message == "" {
				t.Fatal("unreconciled engine started")
			}
			if !strings.Contains(message, "Engine settings could not be reconciled.") || strings.Contains(message, "private launch") {
				t.Fatalf("unsafe start error: %s", message)
			}
			state, err := e.LaunchSettings("fake")
			if err != nil || state.Running || attempts != 1 {
				t.Fatalf("failed recovery state: %+v, attempts=%d, err=%v", state, attempts, err)
			}
			recoverable = true
			if message := start(); message != "" {
				t.Fatal(message)
			}
			state, err = e.LaunchSettings("fake")
			if err != nil || !state.Running || attempts != 2 {
				t.Fatalf("successful recovery state: %+v, attempts=%d, err=%v", state, attempts, err)
			}
		})
	}
}

func TestRecoveryGateAllowsApplyRollbackRestart(t *testing.T) {
	e := settingsExecutor(t, false)
	if err := e.Start(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	request := settingsRequest(t, e)
	request.Settings.LaunchText += " --parallel 3"
	authorizations := 0
	e.settingsParent = func(_ context.Context, method string, p settings.Request, _ string) (json.RawMessage, error) {
		// Recovery here would re-enter ConfigureLaunch while it owns opMu.
		if method != "authorize-start" || p.Engine != "fake" || p.RequestID != "apply-operation" {
			return nil, fmt.Errorf("unexpected recovery during apply")
		}
		authorizations++
		return json.RawMessage(`null`), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := e.ConfigureLaunch(ctx, settings.Configure{Engine: "fake", Settings: request.Settings, OperationID: "apply-operation"}, func(_ context.Context, rollback bool) error {
		if rollback {
			return nil
		}
		return fmt.Errorf("rebind failed")
	})
	if err == nil || !result.Running || !strings.Contains(err.Error(), "previous settings were restored") || authorizations != 1 {
		t.Fatalf("rollback result=%+v err=%v authorizations=%d", result, err, authorizations)
	}
}

func TestRecoveryGateCoversLifecycleStarts(t *testing.T) {
	for _, path := range []string{"start-with", "restart", "restore-enabled", "set-port", "action-restart"} {
		t.Run(path, func(t *testing.T) {
			e := settingsExecutor(t, false)
			if path == "set-port" || path == "action-restart" {
				if err := e.Start(context.Background(), "fake"); err != nil {
					t.Fatal(err)
				}
			}
			if path == "restore-enabled" {
				if err := e.setDesiredEnabled("fake", true); err != nil {
					t.Fatal(err)
				}
			}
			st := settingsState(t, e)
			prepares, authorizations := 0, 0
			e.settingsParent = func(_ context.Context, method string, p settings.Request, _ string) (json.RawMessage, error) {
				switch method {
				case "prepare-start":
					prepares++
					if !st.opMu.TryLock() {
						return nil, fmt.Errorf("recovery attempted under engine lock")
					}
					st.opMu.Unlock()
					return json.RawMessage(`"lifecycle-grant"`), nil
				case "authorize-start":
					authorizations++
					if p.Engine != "fake" || p.RequestID != "lifecycle-grant" {
						return nil, fmt.Errorf("wrong grant")
					}
					return json.RawMessage(`null`), nil
				}
				return nil, fmt.Errorf("unexpected relay")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var err error
			switch path {
			case "start-with":
				err = e.StartWith(ctx, "fake", startOpts{})
			case "restart":
				err = e.Restart(ctx, "fake")
			case "restore-enabled":
				err = e.RestoreEnabled(ctx)
			case "set-port":
				port, portErr := freePort()
				if portErr != nil {
					t.Fatal(portErr)
				}
				_, err = e.SetPort(ctx, "fake", port)
			case "action-restart":
				err = e.restartAfterAction(ctx, st, "fake", "fixture")
			}
			wantAuthorizations := 1
			if path == "set-port" {
				wantAuthorizations = 2 // before persistence, then again at doStart
			}
			if err != nil || prepares != 1 || authorizations != wantAuthorizations {
				t.Fatalf("err=%v prepares=%d authorizations=%d", err, prepares, authorizations)
			}
		})
	}
}

func TestRecoveryGateRejectsMissingBrokerAndRevokedGrant(t *testing.T) {
	for _, mode := range []string{"missing-broker", "revoked-grant"} {
		t.Run(mode, func(t *testing.T) {
			e := settingsExecutor(t, false)
			e.settingsParent = nil
			if mode == "revoked-grant" {
				e.settingsParent = func(_ context.Context, method string, _ settings.Request, _ string) (json.RawMessage, error) {
					if method == "prepare-start" {
						return json.RawMessage(`"revoked"`), nil
					}
					return nil, fmt.Errorf("private launch value")
				}
			}
			err := e.Start(context.Background(), "fake")
			if err == nil || err.Error() != "Engine settings could not be reconciled. This engine is unavailable; restart it to retry." {
				t.Fatalf("unsafe failure: %v", err)
			}
			state, stateErr := e.LaunchSettings("fake")
			if stateErr != nil || state.Running {
				t.Fatal("unauthorized engine started")
			}
		})
	}
}

func TestStartChokepointRefusesMissingAuthorization(t *testing.T) {
	e := settingsExecutor(t, false)
	e.settingsParent = func(context.Context, string, settings.Request, string) (json.RawMessage, error) {
		return nil, fmt.Errorf("not reconciled")
	}
	st := settingsState(t, e)
	st.lockOperation()
	err := e.doStart(context.Background(), st, "fake", startOpts{})
	st.unlockOperation()
	if err == nil {
		t.Fatal("direct start bypassed recovery authorization")
	}
}
