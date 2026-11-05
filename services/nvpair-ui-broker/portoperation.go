// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
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
	finish := b.ownPortOperationWithin(context.Background(), engine, op, portFenceTimeout)
	return func() { _ = finish() }
}

func (b *Broker) ownPortOperationWithin(ctx context.Context, engine string, op *enginePortOperation, fenceBudget time.Duration) func() error {
	op.active.Store(true)
	return func() error {
		defer op.mu.Unlock()
		defer op.active.Store(false)
		if err := b.finishPortOperationWithin(ctx, engine, fenceBudget); err != nil {
			b.engineConfigMu.Lock()
			b.finishSettingsRecoveryLocked(engine, false)
			b.engineConfigMu.Unlock()
			slog.Warn("proxy port convergence failed; engine remains unavailable", "engine", engine)
			return err
		}
		return nil
	}
}

// Caller owns port-op, not engineConfigMu. A ready notification is an observed
// fact, never a new desired configuration. Always compare with the accepted
// journal (or the managed startup choice) after the last worker call, including
// discarded stale results and rollback. Never import an accidental bind into
// persisted settings. Call even when reconciliation otherwise returns early.
func (b *Broker) finishPortOperation(engine string) error {
	return b.finishPortOperationWithin(context.Background(), engine, portFenceTimeout)
}

func (b *Broker) finishPortOperationWithin(ctx context.Context, engine string, fenceBudget time.Duration) error {
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
	_, rpcErr, err := writeProxyPortWithFence(ctx, p, settingsJSON(map[string]int{"port": port}), fenceBudget)
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
