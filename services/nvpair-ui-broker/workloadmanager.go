// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"

	"nvpair-shared/applog"
	"nvpair-shared/jsonrpc"
	"nvpair-shared/noderec"
	"nvpair-ui-broker/relay"
)

// workloadManagerProcess is the broker's handle on its child
// nvpair-workload-manager. Its domain traffic uses notifications: the broker writes local-origin
// workload:* / workloads:remove frames to its stdin (for cluster
// broadcast) and reads workloads:upsert / workloads:remove frames back off
// its stdout (peer-origin events to relay to clients). The shared Peer also
// correlates broker-origin worker/health requests.
//
// The handle owns the OS process, the stdin pipe used to signal clean
// shutdown (via EOF) and to forward frames (workload events, log/set-level),
// and a done channel that closes after cmd.Wait returns.
type workloadManagerProcess struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	// codec stamps every frame the broker writes down the manager's stdin, so
	// the manager's shared jsonrpc reader sees a protocol v2 envelope on each
	// notification. One Codec per child, so its seq is this connection's run.
	codec *Codec
	peer  *Peer
	// stdinMu serializes notification senders; peer shares this codec for
	// health requests, preserving one writer and notification sequence.
	stdinMu sync.Mutex
	done    chan struct{}

	// onNotify, if non-nil, is invoked on the reader goroutine for every
	// notification the manager emits on stdout (workloads:upsert /
	// workloads:remove, plus its startup "ready"). The broker uses it to
	// relay peer-origin workload events to subscribed clients.
	onNotify func(method string, params json.RawMessage)

	// relayDir is the broker's LAN directory. When the manager sends a
	// discovery:subscribe, the broker subscribes it here and pushes
	// discovery:nodes snapshots down via Forward for the manager's peer set. nil
	// when the relay isn't wired.
	relayDir *relay.Directory
	subMu    sync.Mutex
	subID    int
}

// SetLogLevel forwards an already-validated log level to the manager as a
// log/set-level JSON-RPC notification over its stdin. The manager applies
// it via the shared applog stdin handler; it writes no response because we
// send a notification, not an id-bearing request.
func (m *workloadManagerProcess) SetLogLevel(level string) error {
	m.stdinMu.Lock()
	defer m.stdinMu.Unlock()
	return m.codec.Notify(applog.SetLevelMethod, applog.SetLevelParams{Level: level})
}

// Done implements supervisedHandle: the returned channel closes once the
// workload-manager process has exited (cmd.Wait returned).
func (m *workloadManagerProcess) Done() <-chan struct{} { return m.done }

func (m *workloadManagerProcess) PID() int { return m.cmd.Process.Pid }

func (m *workloadManagerProcess) Health(ctx context.Context) error { return healthCall(ctx, m.peer) }

// Forward writes a JSON-RPC notification to the manager's stdin. The broker
// uses it to hand local-origin workload lifecycle / removal frames to the
// manager for cluster broadcast. The canonical codec stamps the frame with this
// connection's epoch and next seq as it writes it, so the manager's reader sees
// a v2 envelope and concurrent senders cannot put the frames on the wire out of
// seq order. The same codec serializes health requests with these writes.
func (m *workloadManagerProcess) Forward(method string, params json.RawMessage) error {
	m.stdinMu.Lock()
	defer m.stdinMu.Unlock()
	return m.codec.Notify(method, params)
}

// startWorkloadManager spawns nvpair-workload-manager with the broker's
// current log level, hides the console window on Windows, and consumes its
// stdout so the broker can relay the manager's outbound notifications.
// onNotify (may be nil) is invoked for every notification the manager
// emits. The manager's stderr is plumbed to the broker's stderr unmodified
// so its [nvpair-workload-manager] applog prefix interleaves with the broker's
// own lines, the same treatment the other workers get.
func startWorkloadManager(binaryPath, logLevel string, relayDir *relay.Directory, onNotify func(method string, params json.RawMessage), extraArgs ...string) (*workloadManagerProcess, error) {
	cmd := exec.Command(binaryPath, append([]string{"--log-level", logLevel}, extraArgs...)...)
	configureSubprocess(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = stderrOut

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("start %s: %w", binaryPath, err)
	}

	peer := NewPeer(jsonrpc.NewCodecMaxFrame(readWriter{stdout, stdin}, 256*1024))
	wm := &workloadManagerProcess{
		cmd:      cmd,
		stdin:    stdin,
		codec:    peer.Codec(),
		peer:     peer,
		done:     make(chan struct{}),
		onNotify: onNotify,
		relayDir: relayDir,
	}

	go wm.readMessages()
	go func() {
		_ = cmd.Wait()
		wm.peer.Close()
		wm.subMu.Lock()
		if wm.subID != 0 && wm.relayDir != nil {
			wm.relayDir.Unsubscribe(wm.subID)
			wm.subID = 0
		}
		wm.subMu.Unlock()
		close(wm.done)
	}()

	return wm, nil
}

// readMessages consumes newline-delimited JSON-RPC frames from the
// manager\'s stdout, correlates responses, and forwards domain notifications
// to onNotify. Discovery subscriptions stay broker-local. It runs until the
// transport closes or a terminal protocol error occurs.
func (m *workloadManagerProcess) readMessages() {
	defer m.stdin.Close()
	m.peer.Serve(nil, func(method string, params json.RawMessage) {
		if method == noderec.MethodSubscribe {
			m.handleSubscribe(params)
			return
		}
		if m.onNotify != nil {
			m.onNotify(method, params)
		}
	})
}

// handleSubscribe wires the manager's discovery:subscribe into the relay
// directory: it registers a subscriber whose Send pushes a discovery:nodes
// snapshot down the manager's stdin (via Forward), then sends the initial
// snapshot so the manager's peer set is populated immediately. A re-subscribe
// drops the prior registration first so it isn't double-fed.
func (m *workloadManagerProcess) handleSubscribe(params json.RawMessage) {
	if m.relayDir == nil {
		return
	}
	send := func(nodes []noderec.DirectoryNode) {
		data, err := json.Marshal(noderec.GetNodesResult{Nodes: nodes})
		if err != nil {
			return
		}
		if err := m.Forward(noderec.NotifyNodes, data); err != nil {
			slog.Debug("failed to push node snapshot to workload-manager", "err", err)
		}
	}
	m.subMu.Lock()
	if m.subID != 0 {
		m.relayDir.Unsubscribe(m.subID)
	}
	id, sub, err := subscribeRelay(m.relayDir, params, send)
	m.subID = id
	m.subMu.Unlock()
	if err != nil {
		slog.Warn("workload-manager sent invalid discovery:subscribe", "err", err)
		return
	}
	m.relayDir.Deliver(sub)
}

// Stop signals the manager to exit by closing its stdin, which the manager
// observes as EOF (spec 12) and treats as its shutdown signal, and waits for it
// to exit, escalating if it does not — see waitForStdinClose. Safe to call
// multiple times.
func (m *workloadManagerProcess) Stop() {
	waitForStdinClose("workload-manager", m.cmd, m.stdin, m.done)
}
