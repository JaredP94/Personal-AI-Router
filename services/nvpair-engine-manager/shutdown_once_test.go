// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// adoptedEngineOnLivePort seeds an engine that this process did not launch and
// cannot stop: running, adopted, no owned proc, and a real listener on its port
// so doStop's readiness probe can never confirm it went away. That is the one
// shape whose stop is refused, leaving st.running true — so a repeated sweep
// does the whole thing again instead of hitting the !st.running early return.
// Its bin path is outside the managed install dir, so the port-reclaim branch
// declines it rather than terminating a stranger's process.
func adoptedEngineOnLivePort(t *testing.T, ex *Executor) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "listen")
	t.Cleanup(func() { _ = ln.Close() })

	st, err := ex.state("fake")
	require.NoError(t, err)
	st.mu.Lock()
	st.running = true
	st.adopted = true
	st.proc = nil
	st.port = ln.Addr().(*net.TCPAddr).Port
	st.binPath = filepath.Join(t.TempDir(), "someone-elses-engine")
	st.mu.Unlock()
}

// TestStopAll_RunsOneSweepPerProcess is the quit-latency guard. An ordinary quit
// asks for StopAll twice — the broker's engine:prepare-shutdown during its
// teardown, and the stdin-EOF path in Run once the broker closes stdin. An
// engine whose stop can only be declined stays running, so each repeat sweep
// would re-pay doStop's readiness probe for it.
func TestStopAll_RunsOneSweepPerProcess(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	adoptedEngineOnLivePort(t, ex)

	ex.StopAll()
	st, err := ex.state("fake")
	require.NoError(t, err)
	st.mu.Lock()
	revision := st.revision
	require.True(t, st.running, "foreign engine must remain running")
	st.mu.Unlock()

	// Count lifecycle transitions, not elapsed time: repeating the sweep must
	// not re-enter the engine operation or probe its foreign listener again.
	ex.StopAll()
	ex.StopAll()
	st.mu.Lock()
	got := st.revision
	st.mu.Unlock()
	require.Equal(t, revision, got, "further StopAll calls repeated the sweep")
}

// TestStopAll_WaitsForTheSweepInFlight is the other half, and the reason this
// is a joining guard rather than a plain skip. A caller that returned while the
// first sweep was still running would report engines stopped before they were:
// the broker would close stdin, engine-manager would exit, and the engines it
// launched would be left running with nothing owning them.
func TestStopAll_WaitsForTheSweepInFlight(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	adoptedEngineOnLivePort(t, ex)
	st, err := ex.state("fake")
	require.NoError(t, err)

	// Holding the engine's opMu parks the sweep before doStop, so it cannot
	// finish until the test lets it, however the goroutines are scheduled.
	st.opMu.Lock()
	releaseSweep := sync.OnceFunc(st.opMu.Unlock)
	t.Cleanup(releaseSweep)
	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		ex.StopAll()
	}()

	// Join behind the first caller only once it owns the sweep: only the sweep
	// sets shuttingDown, and it does so before stopping anything.
	require.Eventually(t, ex.shuttingDown.Load, 5*time.Second, 5*time.Millisecond,
		"the first StopAll caller never started the sweep")
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		ex.StopAll()
	}()

	require.Never(t, func() bool { return isClosed(secondDone) }, 200*time.Millisecond, 10*time.Millisecond,
		"the second caller returned while the first sweep was still running")

	releaseSweep()
	require.Eventually(t, func() bool { return isClosed(sweepDone) }, 10*time.Second, 10*time.Millisecond,
		"the first StopAll caller never returned once the sweep was released")
	require.Eventually(t, func() bool { return isClosed(secondDone) }, 10*time.Second, 10*time.Millisecond,
		"the second StopAll caller never returned once the sweep was released")
}

// isClosed reports whether done has been closed, without blocking.
func isClosed(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// commandDaemon stands in for a command-mode engine's daemon: it serves the
// readiness endpoint once startMarker exists and closes its listener once
// stopMarker does, as a daemon told to stop by its control CLI would. It
// returns the daemon's port.
func commandDaemon(t *testing.T, startMarker, stopMarker string) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fileExists(startMarker) && !fileExists(stopMarker) {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	stopWatch := make(chan struct{})
	t.Cleanup(func() { close(stopWatch) })
	go func() {
		for {
			select {
			case <-stopWatch:
				return
			case <-time.After(50 * time.Millisecond):
				if fileExists(stopMarker) {
					srv.Close()
					return
				}
			}
		}
	}()
	u, err := url.Parse(srv.URL)
	require.NoError(t, err, "parse daemon URL")
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err, "parse daemon port")
	return port
}

// TestStopAll_RetriesAFailedStopCommand checks that a later StopAll caller
// retries an engine whose stop command failed during the sweep, so a stop
// command that errors once does not leave the engine running after quit.
func TestStopAll_RetriesAFailedStopCommand(t *testing.T) {
	dir := t.TempDir()
	startMarker := filepath.Join(dir, "started")
	stopAttempt := filepath.Join(dir, "stop-attempted")
	stopMarker := filepath.Join(dir, "stopped")
	port := commandDaemon(t, startMarker, stopMarker)
	ex := newTestExecutor(t, &Manifest{
		Engine: "daemon", DisplayName: "Daemon Engine", ManifestVersion: 1,
		Platforms: map[string]Platform{
			hostKey(): {
				Detect: []string{fakeEngineBin},
				Runtime: Runtime{
					Mode:  "command",
					Port:  port,
					Start: [][]string{{fakeEngineBin, "touch", startMarker}},
					Stop:  &StopSpec{Cmd: []string{fakeEngineBin, "failoncethentouch", stopAttempt, stopMarker}},
					Ready: &Probe{HTTP: "http://127.0.0.1:{port}/", Status: http.StatusOK, TimeoutS: 10},
				},
			},
		},
	})
	require.NoError(t, ex.Start(context.Background(), "daemon"))
	ex.StopAll()
	st, err := ex.Status("daemon")
	require.NoError(t, err)
	require.True(t, st.Running, "the failed stop must leave the engine running after the sweep")

	ex.StopAll()

	assert.FileExists(t, stopMarker, "the later StopAll must run the stop command again")
	st, err = ex.Status("daemon")
	require.NoError(t, err)
	assert.False(t, st.Running, "the retried stop must leave the engine stopped")
}
