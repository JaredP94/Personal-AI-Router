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
	return rpcErr, err, cancelManagedPortWrite(worker, id)
}

// LM Studio's startup move uses the same worker-owned cancellation join as
// Ollama. It already owns LM Studio port-op, so only this captured worker may
// authorize its prepare-start callback; it must never recursively recover.
func (b *Broker) moveManagedLMStudioPort(ctx context.Context, worker *rpcWorker, port int) (json.RawMessage, *RPCError, error, bool) {
	id := settingsID()
	b.engineConfigMu.Lock()
	if b.managedPortOperations == nil {
		b.managedPortOperations = make(map[string]managedPortAuthorization)
	}
	b.managedPortOperations[id] = managedPortAuthorization{Engine: "lmstudio", Worker: worker}
	b.engineConfigMu.Unlock()
	defer func() {
		b.engineConfigMu.Lock()
		delete(b.managedPortOperations, id)
		b.engineConfigMu.Unlock()
	}()
	result, rpcErr, err := worker.Call(ctx, "internal:set-port", settingsJSON(struct {
		Engine      string `json:"engine"`
		Port        int    `json:"port"`
		OperationID string `json:"operationId"`
	}{"lmstudio", port, id}))
	if err == nil {
		return result, rpcErr, nil, true
	}
	return result, rpcErr, err, cancelManagedPortWrite(worker, id)
}

func cancelManagedPortWrite(worker *rpcWorker, id string) bool {
	fenceCtx, cancel := context.WithTimeout(context.Background(), portFenceTimeout)
	defer cancel()
	_, fenceErr, fenceCallErr := worker.CallNoTimeout(fenceCtx, "internal:cancel-set-port", settingsJSON(map[string]string{"operationId": id}))
	if fenceErr == nil && fenceCallErr == nil {
		return true
	}
	// An unconfirmed join cannot be allowed to persist after a later Apply.
	// Quarantine the captured peer and stop its owned process; keep gates shut.
	worker.peer.Close()
	if worker.cmd != nil && worker.cmd.Process != nil {
		_ = worker.cmd.Process.Kill()
	}
	return false
}

type managedPortAuthorization struct {
	Engine string
	Worker *rpcWorker
	State  ollamaPortReconcileState
}

func (b *Broker) managedPortAuthorizedLocked(engine, id string) bool {
	if id == "" {
		return true
	}
	operation, ok := b.managedPortOperations[id]
	if !ok || operation.Engine != engine {
		return false
	}
	if engine == "lmstudio" {
		return operation.Worker == b.getEngineMgr()
	}
	return operation.State == b.ollamaPortReconcileStateLocked()
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
	finish := b.beginPortOperation("ollama")
	defer finish()
	for {
		b.engineConfigMu.Lock()
		if b.loadEngineSettingsLocked() != nil {
			b.engineConfigMu.Unlock()
			return
		}
		state := b.ollamaPortReconcileStateLocked()
		if state.explicit || !state.managed {
			b.engineConfigMu.Unlock()
			b.reconcileProxyPortOnReadyInOperation(boundPort)
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
			if !b.ollamaPortIOUnlockedLocked(func() { b.setOllamaProxyFallback(status.Port) }) {
				b.engineConfigMu.Unlock()
				continue
			}
			b.recordBlockedOllamaFacade(plan.Blocked)
			b.markOllamaPortReady()
		} else {
			b.managedOllamaBackend.Store(int32(plan.BackendPort))
		}
		b.engineConfigMu.Unlock()
		b.reconcileProxyPortOnReadyInOperation(boundPort)
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
