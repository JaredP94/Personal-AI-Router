// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"sync"
	"testing"
)

func TestHealthStartupConcurrentRuntimeRestore(t *testing.T) {
	e := &Executor{}
	previous := Runtime{Health: &Probe{IntervalS: 1}}
	st := &engineState{plat: &Platform{Runtime: previous}}
	// Stop cancels the old loop but does not join its goroutine. Settings
	// rollback can therefore restore the whole Runtime before that loop
	// has even read its health configuration.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var loops sync.WaitGroup
	for range 1000 {
		loops.Add(1)
		go func() {
			defer loops.Done()
			e.runHealth(ctx, st, "fake", 0, 0)
		}()
		st.mu.Lock()
		st.plat.Runtime = previous
		st.mu.Unlock()
	}
	loops.Wait()
}
