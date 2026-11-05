// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"time"

	settings "nvpair-shared/enginesettings"
	serviceerrors "nvpair-shared/errors"
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
func (b *Broker) recoverEngineSettings() []string {
	b.engineConfigMu.Lock()
	b.settingsRecoveryBlocked = make(map[string]bool)
	loadErr := b.loadEngineSettingsLocked()
	for _, engine := range []string{"ollama", "lmstudio", "omlx"} {
		record := b.engineSettings[engine]
		if loadErr != nil || (record != nil && (record.Snapshot.Phase == "failed" || record.Snapshot.Phase == "applying")) {
			b.settingsRecoveryBlocked[engine] = true
			b.settingsAvailabilityBlockedLocked(engine)
		}
	}
	b.engineConfigMu.Unlock()
	var excluded []string
	for _, engine := range []string{"ollama", "lmstudio", "omlx"} {
		if !b.recoverEngineSettingsForEngine(engine) {
			excluded = append(excluded, engine)
		}
	}
	return excluded
}

// Operation executor, never called inline from a reverse callback. Startup,
// lifecycle entry points, and queued recovery all use the same serialiser.
func (b *Broker) recoverEngineSettingsForEngine(engine string) (recovered bool) {
	finish := b.beginPortOperation(engine)
	defer func() {
		finish()
		b.engineConfigMu.Lock()
		recovered = recovered && !b.settingsRecoveryBlocked[engine]
		b.engineConfigMu.Unlock()
	}()
	return b.recoverEngineSettingsInOperation(engine)
}

// Submitted by prepare-start, but never waits behind a port operation. The
// callback remains fail-closed if an operation won admission in the meantime.
func (b *Broker) tryRecoverEngineSettingsForStart(engine string) (recovered bool) {
	op := b.portOperation(engine)
	if !op.mu.TryLock() {
		return false
	}
	finish := b.ownPortOperation(engine, op)
	defer func() {
		finish()
		b.engineConfigMu.Lock()
		recovered = recovered && !b.settingsRecoveryBlocked[engine]
		b.engineConfigMu.Unlock()
	}()
	return b.recoverEngineSettingsInOperation(engine)
}

func (b *Broker) recoverEngineSettingsInOperation(engine string) bool {
	b.settingsApplyMu.Lock()
	defer b.settingsApplyMu.Unlock()
	b.engineConfigMu.Lock()
	defer b.engineConfigMu.Unlock()
	if b.engineSettingsError != nil {
		b.engineSettingsLoaded = false
		b.engineSettingsError = nil
	}
	return b.recoverEngineSettingsLocked(engine)
}

const settingsRecoveryError = "Engine settings could not be reconciled. This engine is unavailable; restart it to retry."

// Called with both settings locks held. Failure in one engine must not prevent
// another engine's journal entry from being reconciled.
func (b *Broker) recoverEngineSettingsLocked(engine string) bool {
	if b.settingsRecoveryBlocked == nil {
		b.settingsRecoveryBlocked = make(map[string]bool)
	}
	if err := b.loadEngineSettingsLocked(); err != nil {
		b.finishSettingsRecoveryLocked(engine, false)
		return false
	}
	if proxy := b.settingsProxy(engine); proxy != nil {
		proxy.readyMu.Lock()
		retired := proxy.portWritesRetired
		proxy.readyMu.Unlock()
		if retired {
			b.finishSettingsRecoveryLocked(engine, false)
			return false
		}
	}
	record := b.engineSettings[engine]
	if record == nil || (record.Snapshot.Phase != "failed" && record.Snapshot.Phase != "applying") {
		b.finishSettingsRecoveryLocked(engine, true)
		return true
	}
	// Keep polling advertisers and the operation's own advertisement restoration
	// gated while the journal lock is released for the worker call.
	b.settingsRecoveryBlocked[engine] = true
	ctx, cancel := context.WithTimeout(context.Background(), settings.OperationBudget)
	defer cancel()
	var err error
	if record.Snapshot.Phase == "failed" {
		// Converge previous choices without changing the original failed receipt.
		recovery := *record
		recovery.Receipts = make(map[string]settingsReceipt)
		recovery.Resume = false
		err = b.runSettingsOperationLocked(ctx, engine, &recovery)
		if err == nil {
			recovery.Snapshot.Error = "Settings could not be applied; previous settings were restored."
		}
		phase, applied, sequence := record.Snapshot.Phase, record.Snapshot.AppliedRevision, record.Snapshot.Sequence
		record.Snapshot = recovery.Snapshot
		record.Snapshot.Phase = phase
		record.Snapshot.AppliedRevision = applied
		record.Snapshot.Sequence = sequence
		record.Resume = false
		if saveErr := b.saveEngineSettingsLocked(); saveErr != nil {
			err = saveErr
		}
		b.publishSettingsLocked()
	} else {
		// Startup restoration, not replay of an interrupted operation, owns the
		// decision to start a stopped engine after reconciliation succeeds.
		record.Resume = false
		err = b.runSettingsOperationLocked(ctx, engine, record)
	}
	b.finishSettingsRecoveryLocked(engine, err == nil)
	return err == nil
}

func (b *Broker) finishSettingsRecoveryLocked(engine string, recovered bool) {
	if b.settingsRecoveryBlocked == nil {
		b.settingsRecoveryBlocked = make(map[string]bool)
	}
	id := "engine-settings:recovery:" + engine
	if recovered {
		delete(b.settingsRecoveryBlocked, engine)
		b.forwardErrorsClear(id)
		return
	}
	b.settingsRecoveryBlocked[engine] = true
	b.settingsRelayMu.Lock()
	delete(b.settingsStartGrants, engine)
	b.settingsRelayMu.Unlock()
	b.settingsAvailabilityBlockedLocked(engine)
	b.forwardErrorsReport(serviceerrors.ServiceError{
		ID: id, NodeID: b.nodeID, Timestamp: nowMillis(),
		Message: engine + ": " + settingsRecoveryError, Severity: "error", Action: "none",
	})
}

// Advertisers hold engineConfigMu. Suppress even a healthy external listener
// when its settings could not be reconciled; do not claim ownership or stop it.
func (b *Broker) settingsAvailabilityBlockedLocked(engine string) bool {
	if !b.settingsRecoveryBlocked[engine] {
		return false
	}
	b.unregisterService(engineSettingsService(engine))
	b.setProxyLocalBackend(b.settingsProxy(engine), engine, 0, false)
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
