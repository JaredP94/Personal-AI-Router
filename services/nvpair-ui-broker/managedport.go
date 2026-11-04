// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"

	settings "nvpair-shared/enginesettings"
)

// A timed-out RPC is not evidence that the worker abandoned its operation.
// Join cancellation on the captured worker before taking fallback/rollback.
// If fencing cannot be confirmed, keep ownership gated rather than guessing.
func (b *Broker) moveManagedOllamaPort(ctx context.Context, worker *rpcWorker, port int, expected ollamaPortReconcileState) (*RPCError, error, bool) {
	id := settingsID()
	b.engineConfigMu.Lock()
	if expected != b.ollamaPortReconcileStateLocked() {
		b.engineConfigMu.Unlock()
		return &RPCError{Code: -32000, Message: "managed port state changed"}, nil, true
	}
	if b.managedPortOperations == nil {
		b.managedPortOperations = make(map[string]managedPortAuthorization)
	}
	b.managedPortOperations[id] = managedPortAuthorization{Engine: "ollama", State: expected}
	b.engineConfigMu.Unlock()
	defer func() {
		b.engineConfigMu.Lock()
		delete(b.managedPortOperations, id)
		b.engineConfigMu.Unlock()
	}()
	_, rpcErr, err := worker.Call(ctx, "internal:set-port", settingsJSON(struct {
		Engine      string `json:"engine"`
		Port        int    `json:"port"`
		OperationID string `json:"operationId"`
	}{"ollama", port, id}))
	if err == nil {
		return rpcErr, nil, true
	}
	_, fenceErr, fenceCallErr := worker.CallNoTimeout(context.Background(), "internal:cancel-set-port", settingsJSON(map[string]string{"operationId": id}))
	return rpcErr, err, fenceErr == nil && fenceCallErr == nil
}

type managedPortAuthorization struct {
	Engine string
	State  ollamaPortReconcileState
}

func (b *Broker) managedPortAuthorizedLocked(engine, id string) bool {
	if id == "" {
		return true
	}
	operation, ok := b.managedPortOperations[id]
	return ok && operation.Engine == engine && operation.State == b.ollamaPortReconcileStateLocked()
}

// Only values that invalidate the plan belong here; snapshot sequence/health
// refreshes do not. All journal fields are captured under engineConfigMu.
type ollamaPortReconcileState struct {
	epoch           string
	revision        uint64
	phase           string
	config          settings.Config
	explicit        bool
	managed         bool
	pending, source int32
	worker          *rpcWorker
	proxy           *proxyProcess
}

func (b *Broker) ollamaPortReconcileStateLocked() ollamaPortReconcileState {
	state := ollamaPortReconcileState{
		epoch: b.engineSettingsEpoch, managed: b.managedOllamaFacade.Load(),
		pending: b.managedOllamaBackend.Load(), source: b.ollamaBackendPort.Load(),
		worker: b.getEngineMgr(), proxy: b.getProxy(), explicit: b.ollamaExplicitSettings.Load(),
	}
	if r := b.engineSettings["ollama"]; r != nil {
		state.revision, state.phase, state.config = r.Snapshot.Revision, r.Snapshot.Phase, r.Snapshot.Settings
		state.explicit = state.explicit || r.Explicit
	}
	return state
}

// A discarded result can still describe a completed move on an old worker.
// Re-read authoritative state and re-plan; never reuse its consumed candidate.
func (b *Broker) reconcileCurrentOllamaPorts(boundPort int) {
	for {
		b.engineConfigMu.Lock()
		if b.loadEngineSettingsLocked() != nil {
			b.engineConfigMu.Unlock()
			return
		}
		state := b.ollamaPortReconcileStateLocked()
		if state.explicit || !state.managed {
			b.engineConfigMu.Unlock()
			b.reconcileProxyPortOnReady(boundPort)
			return
		}
		if state.worker == nil {
			b.engineConfigMu.Unlock()
			return
		}
		var result json.RawMessage
		var rpcErr *RPCError
		var err error
		current := b.ollamaPortIOUnlockedLocked(func() {
			result, rpcErr, err = state.worker.Call(context.Background(), "engine:status", settingsJSON(map[string]string{"engine": "ollama"}))
		})
		if !current {
			b.engineConfigMu.Unlock()
			continue
		}
		var status ollamaPortStatus
		if err != nil || rpcErr != nil || json.Unmarshal(result, &status) != nil || status.Port <= 0 {
			b.engineConfigMu.Unlock()
			return // no authoritative configuration: retain the ownership gate
		}
		if state.proxy != nil {
			if ready, port := state.proxy.Status(); ready {
				boundPort = port
			}
		}
		plan := planManagedOllamaPorts(true, status, b.availableOffOllamaHostAlias(func(port int) bool {
			return port == managedOllamaFacadePort && boundPort == managedOllamaFacadePort || tcpPortAvailable(port)
		}))
		b.ollamaBackendPort.Store(int32(status.Port))
		if plan.Blocked != "" {
			b.blockManagedOllamaFacade(plan.Blocked, status.Port)
		} else {
			b.managedOllamaBackend.Store(int32(plan.BackendPort))
		}
		b.engineConfigMu.Unlock()
		b.reconcileProxyPortOnReady(boundPort)
		return
	}
}

// Caller owns engineConfigMu, but never lends it to a worker round trip. Even
// set-port's rollback can prepare-start back into this broker. Revalidate after
// every call, and let a fresh pass use current state instead of stale results.
func (b *Broker) ollamaPortIOUnlockedLocked(call func()) bool {
	before := b.ollamaPortReconcileStateLocked()
	b.engineConfigMu.Unlock()
	call()
	b.engineConfigMu.Lock()
	return before == b.ollamaPortReconcileStateLocked()
}
