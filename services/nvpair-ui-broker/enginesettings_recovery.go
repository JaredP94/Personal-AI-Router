// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"time"

	settings "nvpair-shared/enginesettings"
)

func (b *Broker) explicitEngineSettings(engine string) (settings.Config, bool) {
	b.engineConfigMu.Lock()
	defer b.engineConfigMu.Unlock()
	return b.explicitEngineSettingsLocked(engine)
}

func (b *Broker) explicitEngineSettingsLocked(engine string) (settings.Config, bool) {
	if err := b.loadEngineSettingsLocked(); err != nil {
		return settings.Config{}, false
	}
	if record := b.engineSettings[engine]; record != nil && record.Explicit {
		return record.Snapshot.Settings, true
	}
	return settings.Config{}, false
}

func (b *Broker) prepareExplicitEngineSettings(engine string) bool {
	b.engineConfigMu.Lock()
	loadErr := b.loadEngineSettingsLocked()
	b.engineConfigMu.Unlock()
	if loadErr != nil {
		return true
	} // preserve component stores; recovery reports the journal error
	config, ok := b.explicitEngineSettings(engine)
	if !ok {
		return false
	}
	b.setExplicitSettings(engine, true)
	b.disableManagedSettingsFacade(engine)
	b.settingsStartupPort(engine, config.ProxyPort)
	switch engine {
	case "ollama":
		b.ollamaBackendPort.Store(int32(config.ServerPort))
		b.syncCurrentEngineOllamaHostAliasReservation()
	case "lmstudio":
		b.lmstudioBackendPort.Store(int32(config.ServerPort))
	}
	return true
}

// Replay accepted operations before automatic engine startup. Component stores
// can be at any phase after a crash; configure revalidates and converges them
// under the same engine lock used by a live Apply, then records its outcome.
func (b *Broker) recoverEngineSettings() bool {
	// Same order a live Apply uses: the operation lock, then the journal lock
	// that runSettingsOperationLocked releases around the engine restart.
	b.settingsApplyMu.Lock()
	defer b.settingsApplyMu.Unlock()
	b.engineConfigMu.Lock()
	defer b.engineConfigMu.Unlock()
	if err := b.loadEngineSettingsLocked(); err != nil {
		slog.Error("engine settings recovery blocked", "err", err)
		return false
	}
	for engine, record := range b.engineSettings {
		if record.Snapshot.Phase == "failed" {
			// A failed rollback can leave component files unreconciled. Converge
			// the previous choices before allowing any automatic enabled start,
			// but never turn the original failed receipt into a successful apply.
			recovery := *record
			recovery.Receipts = make(map[string]settingsReceipt)
			recovery.Resume = false
			ctx, cancel := context.WithTimeout(context.Background(), settings.OperationBudget)
			err := b.runSettingsOperationLocked(ctx, engine, &recovery)
			cancel()
			if err == nil {
				recovery.Snapshot.Error = "Settings could not be applied; previous settings were restored."
			}
			phase, applied, sequence := record.Snapshot.Phase, record.Snapshot.AppliedRevision, record.Snapshot.Sequence
			record.Snapshot = recovery.Snapshot
			record.Snapshot.Phase = phase
			record.Snapshot.AppliedRevision = applied
			record.Snapshot.Sequence = sequence
			record.Resume = false
			if b.saveEngineSettingsLocked() != nil {
				return false
			}
			b.publishSettingsLocked()
			if err != nil {
				return false
			}
			continue
		}
		if record.Snapshot.Phase != "applying" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), settings.OperationBudget)
		_ = b.runSettingsOperationLocked(ctx, engine, record)
		cancel()
	}
	return true
}

// Legacy lifecycle/port RPCs share the node lock. Reconcile their component
// changes into the same revision stream before allowing another Apply.
func (b *Broker) reconcileLegacySettingsLocked() {
	if !b.engineSettingsLoaded || b.engineSettingsError != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	changed := false
	for engine, record := range b.engineSettings {
		before := record.Snapshot
		after, err := b.settingsSnapshotLocked(ctx, engine)
		if err == nil && before != after {
			changed = true
		}
	}
	if changed {
		b.publishSettingsLocked()
	}
}
