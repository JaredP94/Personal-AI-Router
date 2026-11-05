// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"time"
)

// Downloads stop concurrently with engines. The CLI shutdown sequence has at
// most 500ms signal helper + 500ms grace + 500ms kill helper + 2s reap (3.5s).
// Its 4s aggregate join is below the broker's 6s engine-manager EOF grace;
// StopAll runs alongside it, not afterward. No cache files are touched.
const downloadShutdownBudget = 4 * time.Second
const downloadShutdownSignalBudget = 500 * time.Millisecond
const downloadShutdownGrace = 500 * time.Millisecond
const downloadShutdownReap = 2 * time.Second

func (e *Executor) shutdown() {
	e.shuttingDown.Store(true)
	timer := time.NewTimer(downloadShutdownBudget)
	done := make(chan struct{})
	go func() { e.stopDownloads(); close(done) }()
	e.StopAll()
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

func (e *Executor) stopDownloads() {
	e.pullMu.Lock()
	active := make([]*activePull, 0, len(e.pulls))
	for _, p := range e.pulls {
		active = append(active, p)
	}
	e.pullMu.Unlock()
	timer := time.NewTimer(downloadShutdownBudget)
	defer timer.Stop()
	for _, p := range active {
		p.shutdownOnce.Do(func() { close(p.shutdown) })
		p.cancel()
	}
	for _, p := range active {
		select {
		case <-p.done:
		case <-timer.C:
			return
		}
	}
}

func pullShutdown(ctx context.Context) <-chan struct{} {
	if p, ok := ctx.Value(activePullKey{}).(*activePull); ok {
		return p.shutdown
	}
	return nil
}
