// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// A canceled call is not a canceled write. Join the operation-ID fence on the
// captured worker before releasing port-op; if it cannot acknowledge, retire
// that worker rather than accepting more writes or late ready notifications.
func writeProxyPort(ctx context.Context, p *proxyProcess, params json.RawMessage) (json.RawMessage, *RPCError, error) {
	return writeProxyPortWithFence(ctx, p, params, portFenceTimeout)
}

func writeProxyPortWithFence(ctx context.Context, p *proxyProcess, params json.RawMessage, fenceBudget time.Duration) (json.RawMessage, *RPCError, error) {
	if p == nil || p.peer == nil {
		return nil, nil, fmt.Errorf("proxy unavailable")
	}
	p.readyMu.Lock()
	retired := p.portWritesRetired
	p.readyMu.Unlock()
	if retired {
		return nil, nil, fmt.Errorf("proxy port writes unavailable")
	}
	var request map[string]json.RawMessage
	if json.Unmarshal(params, &request) != nil || request == nil {
		return nil, nil, fmt.Errorf("invalid proxy port request")
	}
	id := settingsID()
	request["operationId"] = settingsJSON(id)
	callCtx, cancel := context.WithTimeout(ctx, proxyCallTimeout)
	defer cancel()
	result, rpcErr, err := p.peer.Call(callCtx, "set-port", settingsJSON(request))
	if err == nil {
		err = callCtx.Err()
	}
	if err != nil {
		fenceCtx, fenceCancel := context.WithTimeout(context.Background(), fenceBudget)
		defer fenceCancel()
		_, fenceErr, fenceCallErr := p.peer.Call(fenceCtx, "internal:cancel-set-port", settingsJSON(map[string]string{"operationId": id}))
		if fenceErr != nil || fenceCallErr != nil {
			p.retirePortWrites()
		}
		return nil, rpcErr, err
	}
	if rpcErr == nil {
		var ready proxyReadyParams
		if json.Unmarshal(result, &ready) == nil && ready.Port > 0 {
			p.readyMu.Lock()
			if p.portWritesRetired {
				p.readyMu.Unlock()
				return nil, nil, fmt.Errorf("proxy port writes unavailable")
			}
			p.port = ready.Port
			p.readyMu.Unlock()
		}
	}
	return result, rpcErr, err
}

// Cancellation has its own short budget: the caller may already be canceled.
// Use the normal control-plane deadline, not an engine restart budget.
const portFenceTimeout = proxyCallTimeout

func (p *proxyProcess) retirePortWrites() {
	p.readyMu.Lock()
	p.portWritesRetired = true
	p.ready = false
	p.readyMu.Unlock()
	p.peer.Close()
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

// Error readers only enqueue this operation; they never acquire port-op.
func (b *Broker) recoverProxyBindFailure(engine string, generation uint64, port int) {
	finish := b.beginPortOperation(engine)
	defer finish()
	b.engineConfigMu.Lock()
	_, explicit := b.explicitEngineSettingsLocked(engine)
	b.engineConfigMu.Unlock()
	if explicit {
		return
	}
	switch engine {
	case "ollama":
		if generation == b.currentOllamaProxyGeneration() && b.managedOllamaFacade.Load() {
			b.blockManagedOllamaFacade("another process acquired the compatibility port during startup")
		}
	case "lmstudio":
		if generation != b.lmstudioProxyGeneration.Load() {
			return
		}
		if b.managedLMStudioFacade.Load() && port == managedLMStudioFacadePort {
			_, _ = b.blockManagedLMStudioFacade("another process acquired the compatibility port during startup", nil)
		} else {
			b.setLMStudioProxyFallback(port)
		}
	case "omlx":
		if generation == b.omlxProxyGeneration.Load() {
			b.setOMLXProxyFallback(port)
		}
	case "llamacpp":
		if generation == b.llamacppProxyGeneration.Load() {
			b.setLlamaCPPProxyFallback(port)
		}
	}
}
