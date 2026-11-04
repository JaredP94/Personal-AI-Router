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
func (b *Broker) prepareEngineStart(ctx context.Context, engine string) (string, error) {
	b.settingsApplyMu.Lock()
	defer b.settingsApplyMu.Unlock()
	b.engineConfigMu.Lock()
	defer b.engineConfigMu.Unlock()
	if ctx.Err() != nil {
		return "", fmt.Errorf("%s", settingsRecoveryError)
	}
	if engine == "ollama" || engine == "lmstudio" || engine == "omlx" {
		if b.engineSettingsError != nil {
			b.engineSettingsLoaded = false
			b.engineSettingsError = nil
		}
		if !b.recoverEngineSettingsLocked(engine) {
			return "", fmt.Errorf("%s", settingsRecoveryError)
		}
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
