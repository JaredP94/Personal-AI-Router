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
	"time"

	"nvpair-shared/applog"
	"nvpair-shared/noderec"
	"nvpair-ui-broker/relay"
)

// proxyCallTimeout bounds how long the broker waits for the proxy to answer a
// relayed control-plane request before giving up.
const proxyCallTimeout = 5 * time.Second

// proxyProcess is the broker's handle on its child ollama-proxy: a full
// bidirectional JSON-RPC peer that runs an HTTP reverse proxy (default :11435),
// emits a "ready" notification carrying the bound port, and accepts id-bearing
// control-plane requests. The id-correlation + read pump live in the shared
// jsonrpc.Peer; this handle owns the OS process and the readiness/port state.
type proxyProcess struct {
	// name identifies which proxy this is ("proxy" / "lmstudio-proxy") in
	// shutdown diagnostics; both engines share this handle type.
	name  string
	cmd   *exec.Cmd
	stdin io.WriteCloser
	peer  *Peer
	done  chan struct{}

	// readyMu guards ready, port, and readyParams, written by the read pump
	// when the proxy's "ready" notification arrives and read by the broker to
	// answer proxy:get-status / replay the baseline on a fresh subscribe.
	readyMu     sync.Mutex
	ready       bool
	port        int
	readyParams json.RawMessage

	// Routing totals mirrored from the proxy's proxy/request events, so
	// proxy:get-status stays answerable locally (no round-trip). The proxy owns
	// the arithmetic — see nvpair-shared/routingmetrics — and these are only the
	// last values it reported. Cleared when the proxy restarts, because a new
	// proxy process starts its own totals at zero.
	routingRequests uint64
	routingHits     uint64

	// onNotify, if non-nil, is invoked for every notification the proxy emits
	// (including "ready") so the broker can forward its event stream.
	onNotify func(method string, params json.RawMessage)

	// relayDir is the broker's LAN directory. When the proxy sends a
	// discovery:subscribe, the broker subscribes it here and pushes
	// discovery:nodes snapshots back down for the proxy's routing set. nil when the
	// relay isn't wired (older call paths / tests).
	relayDir *relay.Directory
	subMu    sync.Mutex
	subID    int
}

// proxyReadyParams mirrors ollama-proxy's "ready" notification payload: the
// proxy binds its port synchronously and echoes it here, the authoritative
// source of the proxy's listen port.
type proxyReadyParams struct {
	Version string `json:"version"`
	Port    int    `json:"port"`
}

// proxyRoutingEventParams reads only the routing totals off a proxy/request
// event. The proxy computed them; the broker mirrors and relays.
type proxyRoutingEventParams struct {
	Requests          uint64 `json:"requests"`
	CacheAffinityHits uint64 `json:"cache_affinity_hits"`
}

// SetLogLevel forwards an already-validated log level as a log/set-level
// notification (through the peer's codec).
func (p *proxyProcess) SetLogLevel(level string) error {
	return p.peer.Notify(applog.SetLevelMethod, applog.SetLevelParams{Level: level})
}

// Done implements supervisedHandle: the returned channel closes once the proxy
// process has exited (cmd.Wait returned).
func (p *proxyProcess) Done() <-chan struct{} { return p.done }

// Status reports whether the proxy has announced itself ready and, if so, the
// HTTP port it bound (0 until "ready" arrives).
func (p *proxyProcess) Status() (bool, int) {
	p.readyMu.Lock()
	defer p.readyMu.Unlock()
	return p.ready, p.port
}

// ReadyParams returns the verbatim payload of the proxy's "ready" notification,
// or nil if it hasn't announced itself yet.
func (p *proxyProcess) ReadyParams() json.RawMessage {
	p.readyMu.Lock()
	defer p.readyMu.Unlock()
	return p.readyParams
}

// routingStatus returns the cumulative routing totals from the proxy's most
// recent proxy/request event. Zero before the first routed request, which is the
// honest "no traffic yet" answer.
func (p *proxyProcess) routingStatus() (requests, cacheAffinityHits uint64) {
	p.readyMu.Lock()
	defer p.readyMu.Unlock()
	return p.routingRequests, p.routingHits
}

// startProxy spawns ollama-proxy with the broker's current log level, hides the
// console window on Windows, and runs the peer read pump to capture the "ready"
// notification (and thus the bound port). onNotify (may be nil) is invoked for
// every notification the proxy emits so the broker can forward its event
// stream. Proxy stderr goes to the broker's non-blocking sink (see
// stderrsink.go), so a stalled reader cannot block the proxy's exit.
func startProxy(name, binaryPath, logLevel string, relayDir *relay.Directory, onNotify func(method string, params json.RawMessage), extraArgs ...string) (*proxyProcess, error) {
	args := append([]string{"--log-level", logLevel}, extraArgs...)
	cmd := exec.Command(binaryPath, args...)
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

	pp := &proxyProcess{
		name:     name,
		cmd:      cmd,
		stdin:    stdin,
		peer:     NewPeer(NewCodec(readWriter{stdout, stdin})),
		done:     make(chan struct{}),
		onNotify: onNotify,
		relayDir: relayDir,
	}

	go func() {
		defer stdin.Close() // read pump termination severs the owned control pipe
		pp.peer.Serve(nil, pp.handleNotify)
	}()
	go func() {
		_ = cmd.Wait()
		pp.peer.Close()
		pp.subMu.Lock()
		if pp.subID != 0 && pp.relayDir != nil {
			pp.relayDir.Unsubscribe(pp.subID)
			pp.subID = 0
		}
		pp.subMu.Unlock()
		close(pp.done)
	}()

	return pp, nil
}

// handleNotify records the proxy's "ready" port and forwards every notification
// to the broker. A malformed "ready" is logged and not forwarded (matching the
// pre-refactor behavior).
func (p *proxyProcess) handleNotify(method string, params json.RawMessage) {
	if method == "ready" {
		var rp proxyReadyParams
		if err := json.Unmarshal(params, &rp); err != nil {
			slog.Warn("proxy emitted invalid ready payload", "err", err)
			return
		}
		p.readyMu.Lock()
		p.ready = true
		p.port = rp.Port
		p.readyParams = append(json.RawMessage(nil), params...)
		p.readyMu.Unlock()
		slog.Info("proxy reported ready", "version", rp.Version, "port", rp.Port)
	}
	if method == "proxy/request" {
		var ev proxyRoutingEventParams
		if err := json.Unmarshal(params, &ev); err != nil {
			slog.Debug("proxy emitted invalid proxy/request routing totals", "err", err)
		} else {
			p.readyMu.Lock()
			p.routingRequests = ev.Requests
			p.routingHits = ev.CacheAffinityHits
			p.readyMu.Unlock()
		}
		// Fall through: the event is still forwarded to clients verbatim.
	}
	// The proxy subscribes upward for its routing targets: wire it to
	// the relay directory and push events down, rather than forwarding this as a
	// client-facing event.
	if method == noderec.MethodSubscribe {
		p.handleSubscribe(params)
		return
	}
	if p.onNotify != nil {
		p.onNotify(method, params)
	}
}

// handleSubscribe wires the proxy's discovery:subscribe into the relay
// directory: it registers a subscriber whose Send pushes a discovery:nodes
// snapshot down the proxy's peer, then sends the initial snapshot so the proxy's
// routing set is populated immediately. A re-subscribe (e.g. the proxy resends)
// drops the prior registration first so it isn't double-fed.
func (p *proxyProcess) handleSubscribe(params json.RawMessage) {
	if p.relayDir == nil {
		return
	}
	send := func(nodes []noderec.DirectoryNode) {
		if err := p.peer.Notify(noderec.NotifyNodes, noderec.GetNodesResult{Nodes: nodes}); err != nil {
			slog.Debug("failed to push node snapshot to proxy", "err", err)
		}
	}
	p.subMu.Lock()
	if p.subID != 0 {
		p.relayDir.Unsubscribe(p.subID)
	}
	id, sub, err := subscribeRelay(p.relayDir, params, send)
	p.subID = id
	p.subMu.Unlock()
	if err != nil {
		slog.Warn("proxy sent invalid discovery:subscribe", "err", err)
		return
	}
	p.relayDir.Deliver(sub)
}

// Call issues an id-bearing control-plane request to the proxy and blocks until
// the response arrives, ctx is cancelled, the proxy exits, or proxyCallTimeout
// elapses.
func (p *proxyProcess) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, *RPCError, error) {
	if p == nil || p.peer == nil {
		return nil, nil, fmt.Errorf("proxy peer not available")
	}
	ctx, cancel := context.WithTimeout(ctx, proxyCallTimeout)
	defer cancel()
	return p.peer.Call(ctx, method, params)
}

// Stop signals the proxy to exit by closing its stdin (observed as EOF; the
// proxy drains its HTTP server) and waits for it to exit, escalating if it does
// not — see waitForStdinClose. Safe to call multiple times.
func (p *proxyProcess) Stop() {
	waitForStdinClose(p.name, p.cmd, p.stdin, p.done)
}
