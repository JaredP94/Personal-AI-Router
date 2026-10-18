// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// telemetryDesiredConfig mirrors the settings worker's authoritative snapshot.
// Keeping this wire type here avoids importing a worker's main package.
type telemetryDesiredConfig struct {
	Revision              uint64 `json:"revision"`
	Enabled               bool   `json:"enabled"`
	CollectorPort         int    `json:"collectorPort"`
	RecordPayloads        bool   `json:"recordPayloads"`
	PayloadConsentVersion int    `json:"payloadConsentVersion"`
	PayloadPolicyVersion  int    `json:"payloadPolicyVersion"`
}

type workerTelemetryState struct {
	Name              string  `json:"name"`
	AppliedRevision   uint64  `json:"appliedRevision"`
	State             string  `json:"state"`
	Error             string  `json:"error"`
	LastExportOK      *string `json:"lastExportOk"`
	LastExportError   *string `json:"lastExportError"`
	LastExportErrorAt *string `json:"lastExportErrorAt"`
}

type telemetryApplication struct {
	proxy      *proxyProcess
	generation uint64
	state      workerTelemetryState
}

var telemetryProxyNames = [...]string{"proxy", "lmstudio-proxy", "omlx-proxy"}

func (b *Broker) telemetryProxies() [3]*proxyProcess {
	b.workersMu.Lock()
	defer b.workersMu.Unlock()
	return [3]*proxyProcess{b.proxy, b.lmstudioProxy, b.omlxProxy}
}

// desiredTelemetryLocked supplies the fail-closed policy before settings is ready.
func (b *Broker) desiredTelemetryLocked() telemetryDesiredConfig {
	if b.telemetryDesired.CollectorPort == 0 {
		return telemetryDesiredConfig{CollectorPort: 4317, PayloadPolicyVersion: 1}
	}
	return b.telemetryDesired
}

func (b *Broker) cacheDesiredTelemetryLocked(config telemetryDesiredConfig) {
	current := b.desiredTelemetryLocked()
	// A delayed startup read must not undo a later change notification.
	if config.Revision < current.Revision || config == current {
		return
	}
	b.telemetryDesired = config
	b.telemetryGeneration++
}

func (b *Broker) loadTelemetryConfig(settings *rpcWorker) {
	if settings == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, rpcErr, err := settings.Call(ctx, "settings/get-telemetry-config", nil)
	if err != nil || rpcErr != nil {
		return
	}
	var config telemetryDesiredConfig
	if json.Unmarshal(result, &config) != nil || config.CollectorPort < 1 || config.CollectorPort > 65535 {
		return
	}
	// Serialize publication against settings replacement. Notification snapshots
	// use the same revision guard as this startup read.
	b.telemetryConfigMu.Lock()
	b.workersMu.Lock()
	if b.settings == settings {
		b.cacheDesiredTelemetryLocked(config)
	}
	b.workersMu.Unlock()
	b.telemetryConfigMu.Unlock()
	b.reconcileTelemetry()
}

// reconcileTelemetry serializes configure requests, but never holds the desired
// lock over I/O. If desired changes during a call, discard that completion and
// loop over the newest snapshot before allowing another reconciler to run.
func (b *Broker) reconcileTelemetry() {
	b.telemetryReconcileMu.Lock()
	defer b.telemetryReconcileMu.Unlock()
	for {
		b.telemetryConfigMu.Lock()
		desired, generation := b.desiredTelemetryLocked(), b.telemetryGeneration
		b.telemetryConfigMu.Unlock()
		proxies := b.telemetryProxies()
		params, _ := json.Marshal(desired)
		for i, p := range proxies {
			if p == nil {
				continue
			}
			if ready, _ := p.Status(); !ready {
				continue
			}
			b.telemetryConfigMu.Lock()
			previous := b.telemetryApplications[i]
			stale := b.telemetryGeneration != generation
			b.telemetryConfigMu.Unlock()
			if stale {
				break
			}
			if previous.proxy == p && previous.generation == generation && previous.state.State == "applied" {
				continue
			}
			state := workerTelemetryState{Name: telemetryProxyNames[i], State: "pending"}
			if previous.proxy == p {
				state.AppliedRevision = previous.state.AppliedRevision
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			raw, rpcErr, err := p.Call(ctx, "telemetry/configure", params)
			cancel()
			var result struct {
				Revision *uint64 `json:"revision"`
			}
			switch {
			case err != nil:
				state.State, state.Error = "failed", err.Error()
			case rpcErr != nil:
				state.State, state.Error = "failed", rpcErr.Message
			case json.Unmarshal(raw, &result) != nil || result.Revision == nil:
				state.State, state.Error = "failed", "invalid telemetry/configure response"
			case *result.Revision != desired.Revision:
				state.State, state.Error = "failed", fmt.Sprintf("proxy reports revision %d, wanted %d", *result.Revision, desired.Revision)
			default:
				state.State, state.AppliedRevision = "applied", *result.Revision
			}
			b.telemetryConfigMu.Lock()
			if b.telemetryGeneration == generation && b.telemetryProxies()[i] == p {
				b.telemetryApplications[i] = telemetryApplication{proxy: p, generation: generation, state: state}
			}
			b.telemetryConfigMu.Unlock()
		}
		b.telemetryConfigMu.Lock()
		changed := b.telemetryGeneration != generation
		b.telemetryConfigMu.Unlock()
		if !changed {
			return
		}
	}
}

func (b *Broker) telemetrySnapshot() (telemetryDesiredConfig, []workerTelemetryState, [3]*proxyProcess) {
	b.telemetryConfigMu.Lock()
	defer b.telemetryConfigMu.Unlock()
	proxies := b.telemetryProxies()
	states := make([]workerTelemetryState, len(proxies))
	for i, p := range proxies {
		state := workerTelemetryState{Name: telemetryProxyNames[i], State: "unavailable"}
		if p != nil {
			state.State = "pending"
			applied := b.telemetryApplications[i]
			if applied.proxy == p {
				state.AppliedRevision = applied.state.AppliedRevision
				if applied.generation == b.telemetryGeneration {
					state = applied.state
				}
			}
			if ready, _ := p.Status(); !ready {
				state.State = "pending"
			}
		}
		states[i] = state
	}
	return b.desiredTelemetryLocked(), states, proxies
}

func (b *Broker) queryTelemetryHealth(ctx context.Context, states []workerTelemetryState, proxies [3]*proxyProcess) {
	for i, p := range proxies {
		if p == nil {
			continue
		}
		if ready, _ := p.Status(); !ready {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		raw, rpcErr, err := p.Call(callCtx, "telemetry/status", nil)
		cancel()
		if err != nil || rpcErr != nil {
			continue
		}
		var health struct {
			LastExportOK      *string `json:"lastExportOk"`
			LastExportError   *string `json:"lastExportError"`
			LastExportErrorAt *string `json:"lastExportErrorAt"`
		}
		if json.Unmarshal(raw, &health) != nil {
			continue
		}
		states[i].LastExportOK = health.LastExportOK
		states[i].LastExportError = health.LastExportError
		states[i].LastExportErrorAt = health.LastExportErrorAt
	}
}
