// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"nvpair-tui/rpc"
	"nvpair-tui/ui"
)

// brokerName is the worker binary the TUI owns. We resolve it next to our
// own executable (the installed bin/ layout puts nvpair-tui beside
// nvpair-ui-broker and all the workers it in turn spawns).
const brokerName = "nvpair-ui-broker"

// shutdownGrace is how long we wait for the broker to exit after asking it to
// shut down (and closing its stdin) before force-killing.
//
// The broker's own teardown is bounded by teardownBudget (everything from the
// start of shutdown through its last worker join) plus
// workloadHistoryFlushJoinTimeout (the final workload-history flush) — both in
// nvpair-ui-broker. This must exceed that sum, or we kill a broker that is still
// legitimately inside its own budget, orphaning whatever workers remain and
// dropping the history flush the budget specifically reserves room for.
//
// Those two currently total 15s, which is also what the desktop parent allows.
// This clock starts after the shutdown request rather than before it, so it needs
// a little headroom on top. If teardownBudget changes, this has to move with it.
const shutdownGrace = 18 * time.Second

// epochSignalDepth bounds how many announced protocol epochs wait for the UI.
// One is enough: the UI acts on "the epoch changed" and the newest epoch is the
// only one worth acting on, so an undrained epoch is replaced rather than
// queued. The depth exists so publishing never has to block the reader loop.
const epochSignalDepth = 1

// Supervisor owns a single nvpair-ui-broker child process and the JSON-RPC
// client speaking to it over stdio. The broker, in turn, spawns and
// supervises the rest of the NVPAIR fleet, so owning the broker means owning
// everything.
type Supervisor struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	Client *rpc.Client
	Stderr io.ReadCloser

	// epochChanges carries protocol epochs the broker announced after Spawn
	// registered its consumer. The UI drains it to re-fetch cached snapshots
	// whenever a different broker process takes over the connection. Receive-only
	// so nothing outside the supervisor can feed the UI a fabricated epoch.
	epochChanges <-chan string
}

// EpochChanges returns the protocol epochs announced on this connection, for
// the UI to watch. See watchEpoch.
func (s *Supervisor) EpochChanges() <-chan string { return s.epochChanges }

// watchEpoch registers the epoch consumer on client and returns the channel of
// changes delivered to it. This is the consumer that makes the broker's
// broker:epoch announcement do something: without it the client records the
// epoch, nobody reads it, and a broker restart leaves every cached roster in the
// UI stale and looking fresh.
//
// Registration belongs on the line before Client.Run, not after it: the broker
// stamps every notification with its epoch and announces broker:epoch on the
// first heartbeat tick, so a frame can be in flight the moment the read loop
// starts. Registering after the loop is running leaves a window where a restart
// during startup is recorded in the client and never reaches the UI.
//
// The callback runs on the client's read loop, outside its epoch lock, so it
// must never block: publishEpoch hands the epoch over without waiting.
func watchEpoch(client *rpc.Client) <-chan string {
	changes := make(chan string, epochSignalDepth)
	client.OnEpochChange(func(epoch string) { publishEpoch(changes, epoch) })
	return changes
}

// publishEpoch hands epoch to changes without ever blocking the caller. If the
// UI has not drained the pending epoch, that one is dropped and this newer one
// takes its place: the re-fetch it triggers reads current state either way, so
// the newest epoch is the only one worth holding.
//
// The read loop is the sole producer, so the second send cannot find the channel
// full again after the receive emptied it.
func publishEpoch(changes chan string, epoch string) {
	select {
	case changes <- epoch:
	default:
		select {
		case <-changes:
		default:
		}
		select {
		case changes <- epoch:
		default:
		}
	}
}

// resolveBrokerPath honours an explicit override, otherwise looks for the
// broker binary alongside this executable. We deliberately do not fall
// back to PATH: the TUI ships in the same bin/ dir as the broker, and a
// stray PATH match for a stale dev build is worse than a clear error.
func resolveBrokerPath(override string) (string, error) {
	if override != "" {
		if _, err := os.Stat(override); err != nil {
			return "", fmt.Errorf("broker binary %q not accessible: %w", override, err)
		}
		return override, nil
	}
	bin := brokerName
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate own executable: %w", err)
	}
	candidate := filepath.Join(filepath.Dir(exe), bin)
	if _, err := os.Stat(candidate); err != nil {
		return "", fmt.Errorf("%s not found next to nvpair-tui (use --broker-path to override): %w", bin, err)
	}
	return candidate, nil
}

// Spawn launches the broker and starts the JSON-RPC read loop. The child
// runs with its working directory set to the broker's own directory so
// the broker's sibling-binary worker resolution finds nvpair-node-scanner et
// al. ctx governs the client read loop; use Shutdown for an orderly stop.
func Spawn(ctx context.Context, brokerPath string) (*Supervisor, error) {
	cmd := exec.Command(brokerPath)
	cmd.Dir = filepath.Dir(brokerPath)
	configureSubprocess(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("broker stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("broker stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("broker stderr: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start broker: %w", err)
	}

	client := rpc.NewClient(stdout, stdin)
	// Before Run: a broker:epoch frame can arrive on the first heartbeat tick,
	// and an unregistered callback would swallow it. See watchEpoch. With this
	// consumer in place the UI learns a different broker process took over and
	// re-reads the snapshots it cached from the old one.
	epochChanges := watchEpoch(client)

	go client.Run(ctx)

	return &Supervisor{cmd: cmd, stdin: stdin, Client: client, Stderr: stderr, epochChanges: epochChanges}, nil
}

// Run constructs fresh views for this owned client lifetime.
func (s *Supervisor) Run() error { return ui.Run(s.Client, s.Stderr, s.EpochChanges()) }

// Shutdown asks the broker to stop cleanly: send the shutdown RPC, close
// its stdin (a second, EOF-based stop signal), then wait up to
// shutdownGrace before killing it. The broker tears its own workers down
// in response, so this leaves no orphans.
func (s *Supervisor) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, _ = s.Client.Call(ctx, "shutdown", nil)
	cancel()

	_ = s.stdin.Close()

	done := make(chan struct{})
	go func() {
		_ = s.cmd.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(shutdownGrace):
		_ = s.cmd.Process.Kill()
		<-done
	}
}
