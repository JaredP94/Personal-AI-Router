// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Cancel only asks the adapter to stop. Confirmation is emitted by trackedPull
// after the adapter has released its transfer. Shared engine caches are never
// inspected or removed; llama.cpp removes only its operation-owned temp file.
// A cancellation always addresses one operation, never its replacement.
type activePull struct {
	engine, model, id string
	cancel            context.CancelFunc
	requested         atomic.Bool
	canCancel         bool
	shutdown          chan struct{}
	done              chan struct{}
	shutdownOnce      sync.Once
}
type activePullKey struct{}

// pullCancelAcceptTimeout bounds the remote acceptance call, not transfer stop.
// Broker waits 10s and desktop 15s. LM Studio's independent stop graces are in
// lmspull.go; they do not extend the acceptance request.
const pullCancelAcceptTimeout = 5 * time.Second

func (e *Executor) CancelModelPull(engine, model, opID string) bool {
	if opID == "" {
		return false
	}
	e.pullMu.Lock()
	p := e.pulls[opID]
	if p == nil || p.engine != engine || p.model != model || !p.canCancel {
		e.pullMu.Unlock()
		return false
	}
	p.requested.Store(true)
	e.pullMu.Unlock()
	p.cancel() // context cancellation and every callback happen outside pullMu.
	return true
}

// finishPull fences the adapter's terminal decision against operation-ID cancel
// acceptance. It never holds pullMu across filesystem or network I/O.
func (e *Executor) finishPull(ctx context.Context) bool {
	p, ok := ctx.Value(activePullKey{}).(*activePull)
	if !ok {
		return false
	}
	e.pullMu.Lock()
	defer e.pullMu.Unlock()
	p.canCancel = false
	return p.requested.Load()
}

func (e *Executor) trackedPull(ctx context.Context, engine, model, opID string, canCancel bool, run func(context.Context) (json.RawMessage, error)) (json.RawMessage, error) {
	if model == "" {
		return nil, fmt.Errorf("model is required")
	}
	if opID == "" {
		opID = newOpID()
	}
	ctx, cancel := context.WithTimeout(ctx, e.actionTimeout)
	defer cancel()
	p := &activePull{engine: engine, model: model, id: opID, cancel: cancel, canCancel: canCancel, shutdown: make(chan struct{}), done: make(chan struct{})}
	defer close(p.done)
	ctx = context.WithValue(ctx, activePullKey{}, p)
	e.pullMu.Lock()
	if e.shuttingDown.Load() {
		e.pullMu.Unlock()
		return nil, context.Canceled
	}
	if e.pulls == nil {
		e.pulls = make(map[string]*activePull)
	}
	for _, active := range e.pulls {
		if active.id == opID || (active.engine == engine && active.model == model) {
			e.pullMu.Unlock()
			return nil, fmt.Errorf("download already active")
		}
	}
	e.pulls[opID] = p
	e.pullMu.Unlock()
	e.emitPullUpdate(ctx, ProgressEvent{Stage: "pulling", Message: model})
	result, err := run(ctx)
	stage, message := "done", ""
	if e.shuttingDown.Load() {
		stage, message = "interrupted", "Download interrupted — retry to resume"
		result, err = json.RawMessage(`{"status":"interrupted"}`), nil
	} else if p.requested.Load() && errors.Is(err, context.Canceled) {
		stage = "cancelled"
		message = "Partial data is kept so the download can resume."
		if engine == "llamacpp" {
			message = "Download stopped; this operation's temporary file was removed."
		}
		result, err = json.RawMessage(`{"status":"cancelled"}`), nil
	} else if err != nil {
		stage = "error"
		message = e.reportPullFailed(engine, model, err)
	}
	e.pullMu.Lock()
	if e.pulls[opID] == p {
		delete(e.pulls, opID)
	}
	e.pullMu.Unlock()
	percent := 0
	if stage == "error" {
		percent = -1
	}
	e.emitPullProgress(ProgressEvent{Engine: engine, Model: model, OpID: opID, Op: "pull", Stage: stage, Message: message, Percent: percent})
	return result, err
}

func (e *Executor) emitPullUpdate(ctx context.Context, ev ProgressEvent) {
	p, ok := ctx.Value(activePullKey{}).(*activePull)
	if !ok || ctx.Err() != nil {
		return
	}
	ev.Engine, ev.Model, ev.OpID, ev.Op, ev.CanCancel = p.engine, p.model, p.id, "pull", p.canCancel
	e.emitPullProgress(ev)
}
