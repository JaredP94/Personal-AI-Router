// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"

	settings "nvpair-shared/enginesettings"
)

// Recovery remains broker-owned: only this process owns the journal and proxy
// transaction. The worker prepares outside opMu, then checks this generation
// again at its launch chokepoint. A configure revokes ordinary start grants.
func (b *Broker) prepareEngineStart(ctx context.Context, engine, operationID string) (string, error) {
	b.engineConfigMu.Lock()
	defer b.engineConfigMu.Unlock()
	if ctx.Err() != nil || !b.managedPortAuthorizedLocked(engine, operationID) {
		return "", fmt.Errorf("%s", settingsRecoveryError)
	}
	if engine == "ollama" || engine == "lmstudio" || engine == "omlx" {
		loadErr := b.loadEngineSettingsLocked()
		record := b.engineSettings[engine]
		needsRecovery := loadErr != nil || b.settingsRecoveryBlocked[engine] || (record != nil && (record.Snapshot.Phase == "failed" || record.Snapshot.Phase == "applying"))
		if needsRecovery {
			// A reverse callback must never acquire port-op, or wait for its
			// parent operation. Fail closed while any operation owns this engine.
			if operationID != "" || b.portOperation(engine).active.Load() {
				return "", fmt.Errorf("%s", settingsRecoveryError)
			}
			// Recovery is a separate operation, not work done under a callback
			// lock. Ordinary prepare occurs before the worker takes opMu.
			done := make(chan bool, 1)
			go func() { done <- b.tryRecoverEngineSettingsForStart(engine) }()
			b.engineConfigMu.Unlock()
			var recovered bool
			select {
			case recovered = <-done:
			case <-ctx.Done():
			}
			b.engineConfigMu.Lock()
			if !recovered {
				return "", fmt.Errorf("%s", settingsRecoveryError)
			}
		}
	}
	if ctx.Err() != nil || b.settingsRecoveryBlocked[engine] || !b.managedPortAuthorizedLocked(engine, operationID) {
		return "", fmt.Errorf("%s", settingsRecoveryError)
	}
	b.settingsRelayMu.Lock()
	defer b.settingsRelayMu.Unlock()
	if b.settingsStartGrants == nil {
		b.settingsStartGrants = make(map[string]string)
	}
	if b.settingsStartGrants[engine] == "" {
		if len(b.settingsStartGrants) >= 128 {
			return "", fmt.Errorf("%s", settingsRecoveryError)
		}
		b.settingsStartGrants[engine] = settingsID()
	}
	return b.settingsStartGrants[engine], nil
}

// Called while the worker owns opMu. Never take settingsApplyMu/engineConfigMu
// or perform worker I/O here: an apply may own those locks and wait for opMu.
// Active configure tokens authorize both its candidate and rollback restart.
func (b *Broker) authorizeEngineStart(p settings.Request) error {
	b.settingsRelayMu.Lock()
	defer b.settingsRelayMu.Unlock()
	operation, active := b.activeSettings[p.RequestID]
	if p.RequestID != "" && ((active && operation.Engine == p.Engine) || b.settingsStartGrants[p.Engine] == p.RequestID) {
		return nil
	}
	return fmt.Errorf("%s", settingsRecoveryError)
}
