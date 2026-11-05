// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
)

// Lock order: port-op -> engineConfigMu, NEVER the reverse. The per-engine
// serialiser owns the entire operation, including worker calls and compensation.
// Reverse callbacks (prepare/authorize-start, rebind, rollback) NEVER acquire
// it: rebind/rollback borrow their active parent operation's ownership. Ready
// and error readers only enqueue work; they must remain able to read replies.
type enginePortOperation struct {
	mu     sync.Mutex
	active atomic.Bool
}

func (b *Broker) portOperation(engine string) *enginePortOperation {
	value, _ := b.portOperations.LoadOrStore(engine, &enginePortOperation{})
	return value.(*enginePortOperation)
}

func (b *Broker) beginPortOperation(engine string) func() {
	op := b.portOperation(engine)
	op.mu.Lock()
	return b.ownPortOperation(engine, op)
}

func (b *Broker) ownPortOperation(engine string, op *enginePortOperation) func() {
	op.active.Store(true)
	return func() {
		defer op.mu.Unlock()
		defer op.active.Store(false)
		if err := b.finishPortOperation(engine); err != nil {
			b.engineConfigMu.Lock()
			b.finishSettingsRecoveryLocked(engine, false)
			b.engineConfigMu.Unlock()
			slog.Warn("proxy port convergence failed; engine remains unavailable", "engine", engine)
		}
	}
}

// Caller owns port-op, not engineConfigMu. A ready notification is an observed
// fact, never a new desired configuration. Always compare with the accepted
// journal (or the managed startup choice) after the last worker call, including
// discarded stale results and rollback. Never import an accidental bind into
// persisted settings. Call even when reconciliation otherwise returns early.
func (b *Broker) finishPortOperation(engine string) error {
	b.engineConfigMu.Lock()
	port := b.proxyStartupPort(engine)
	if r := b.engineSettings[engine]; r != nil && r.Explicit {
		port = r.Snapshot.Settings.ProxyPort
	}
	p := b.settingsProxy(engine)
	b.engineConfigMu.Unlock()
	if port <= 0 || p == nil {
		return nil // startup has not published a proxy yet
	}
	ready, actual := p.Status()
	if !ready || actual == port {
		return nil
	}
	_, rpcErr, err := writeProxyPort(context.Background(), p, settingsJSON(map[string]int{"port": port}))
	if err != nil {
		return err
	}
	if rpcErr != nil {
		return fmt.Errorf("proxy port convergence rejected")
	}
	return nil
}

func (b *Broker) proxyStartupPort(engine string) int {
	switch engine {
	case "ollama":
		return int(b.ollamaProxyStartupPort.Load())
	case "lmstudio":
		return int(b.lmstudioProxyStartupPort.Load())
	case "omlx":
		return int(b.omlxProxyStartupPort.Load())
	}
	return 0
}
