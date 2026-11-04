// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
)

// Unversioned proxy writes must join their reply (or worker exit), not merely
// a local timeout: otherwise a late bind could outlive port-op ownership.
// Reverse configure callbacks borrow the parent operation; the parent joins
// those callbacks before its final convergence and before releasing port-op.
func writeProxyPort(p *proxyProcess, params json.RawMessage) (json.RawMessage, *RPCError, error) {
	if p == nil || p.peer == nil {
		return nil, nil, fmt.Errorf("proxy unavailable")
	}
	result, rpcErr, err := p.peer.Call(context.Background(), "set-port", params)
	if err == nil && rpcErr == nil {
		var ready proxyReadyParams
		if json.Unmarshal(result, &ready) == nil && ready.Port > 0 {
			p.readyMu.Lock()
			p.port = ready.Port
			p.readyMu.Unlock()
		}
	}
	return result, rpcErr, err
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
	}
}
