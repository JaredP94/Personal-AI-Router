// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"runtime"
	"sync"
	"testing"
)

// A passive probe can outlive its revision and overlap settings rollback.
// Revision fencing must not leave detectPath reading the mutable Runtime.
func TestPassiveStatusConcurrentRuntimeRestore(t *testing.T) {
	for _, detect := range []bool{false, true} {
		name := "status"
		if detect {
			name = "detect"
		}
		t.Run(name, func(t *testing.T) {
			e := newTestExecutor(t, testEngineManifest(fakeEngineForTest(t)))
			st, err := e.state("fake")
			if err != nil {
				t.Fatal(err)
			}
			// Command mode exercises both Mode and CLI in detectedExecutable.
			st.plat.Runtime.Mode = "command"
			st.plat.Runtime.CLI = fakeEngineForTest(t)
			previous := st.plat.Runtime
			// Use rollback's locks and whole-value assignment. Leave opDone
			// clear to exercise an already-admitted passive probe on every read.
			var writer sync.WaitGroup
			start, stop := make(chan struct{}), make(chan struct{})
			writer.Add(1)
			go func() {
				defer writer.Done()
				st.opMu.Lock()
				defer st.opMu.Unlock()
				close(start)
				for {
					select {
					case <-stop:
						return
					default:
					}
					st.mu.Lock()
					st.plat.Runtime = previous
					st.mu.Unlock()
					runtime.Gosched()
				}
			}()
			<-start
			defer writer.Wait()
			defer close(stop)
			for range 100 {
				var installed bool
				if detect {
					installed, err = e.Detect("fake")
				} else {
					var status EngineStatus
					status, err = e.statusWithContext(context.Background(), "fake", 0, false)
					installed = status.Installed
				}
				if err != nil || !installed {
					t.Fatalf("passive detection lost installed engine: installed=%v err=%v", installed, err)
				}
			}
		})
	}
}

func TestHealthStartupWithoutProbe(t *testing.T) {
	st := &engineState{plat: &Platform{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	(&Executor{}).runHealth(ctx, st, "fake", 0, 0)
}
