// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"

	"nvpair-shared/errors"
	"nvpair-shared/listen"
)

// proxyport.go owns the broker's proxy/engine port ordering. With managed
// ownership enabled it reserves :11434, then moves a stopped backend when its
// configured port is the facade or is occupied. A running or unknown owner is never killed.
// With managed ownership disabled the legacy engine-wins bump remains.

// proxyPortBumpedID is the sticky warning id surfaced when the broker moves
// the proxy off a port a running engine holds. Sticky (no timestamp suffix)
// so repeated bumps upsert one entry; cleared when the user later picks a
// proxy port that doesn't collide.
const (
	proxyPortBumpedID         = "ollama-proxy:port-bumped"
	portOwnershipBlockedID    = "ollama-proxy:port-ownership-blocked"
	managedOllamaFacadePort   = 11434
	managedOllamaBackendStart = 11435
)

type ollamaPortStatus struct {
	Running bool `json:"running"`
	Port    int  `json:"port"`
}

type managedPortPlan struct {
	Enabled     bool
	BackendPort int
	Blocked     string
}

// planManagedOllamaPorts is the policy core kept separate from RPC so every
// safety branch is deterministic in tests. It never plans a move for a
// running engine; a stopped backend whose configured port is occupied advances
// to the next available managed backend port.
func planManagedOllamaPorts(enabled bool, st ollamaPortStatus, available func(int) bool) managedPortPlan {
	if !enabled {
		return managedPortPlan{}
	}
	if st.Running && (st.Port == managedOllamaFacadePort || st.Port == 0) {
		return managedPortPlan{Blocked: "Ollama is already running on the compatibility port"}
	}
	if !available(managedOllamaFacadePort) {
		return managedPortPlan{Blocked: "the compatibility port is already in use"}
	}
	if st.Port > 0 && st.Port != managedOllamaFacadePort {
		if !st.Running && !available(st.Port) {
			backend := nextAvailablePort(managedOllamaBackendStart, available)
			if backend == 0 {
				return managedPortPlan{Blocked: "no free backend port is available"}
			}
			return managedPortPlan{Enabled: true, BackendPort: backend}
		}
		return managedPortPlan{Enabled: true}
	}
	backend := nextAvailablePort(managedOllamaBackendStart, available)
	if backend == 0 {
		return managedPortPlan{Blocked: "no free backend port is available"}
	}
	return managedPortPlan{Enabled: true, BackendPort: backend}
}

func nextAvailablePort(start int, available func(int) bool) int {
	for port := start; port <= 65535; port++ {
		if available(port) {
			return port
		}
	}
	return 0
}

func nextAvailablePortExcluding(start int, excludedPorts []int, available func(int) bool) int {
	excluded := map[int]bool{}
	for _, port := range excludedPorts {
		excluded[port] = true
	}
	return nextAvailablePort(start, func(port int) bool {
		return !excluded[port] && available(port)
	})
}

// tcpPortAvailable uses the same wildcard and loopback reservation as the proxies. The
// listener is only a preflight; the proxy's bind remains authoritative and a
// process winning the race is handled as a normal, non-destructive failure.
func tcpPortAvailable(port int) bool {
	ln, err := listen.TCP(fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

func (b *Broker) reportPortOwnershipBlocked(reason string) {
	b.forwardErrorsReport(errors.ServiceError{
		ID:        portOwnershipBlockedID,
		Message:   fmt.Sprintf("NVPAIR could not safely reserve port %d: %s. No running or unknown process was stopped.", managedOllamaFacadePort, reason),
		Timestamp: nowMillis(),
		NodeID:    b.nodeID,
		Severity:  "warning",
		Action:    "none",
	})
}

// blockManagedOllamaFacade disables the managed attempt and chooses an
// explicit free fallback for the proxy. Explicit startup matters here: a
// persisted :11434 must not make a blocked proxy crash-loop behind an existing
// owner. It returns the fallback so a live proxy can rebind there too.
func (b *Broker) blockManagedOllamaFacade(reason string, excludedPorts ...int) int {
	fallback := b.blockManagedOllamaFacadePending(reason, excludedPorts...)
	b.markOllamaPortReady()
	return fallback
}

// The live move owner releases the gate only after cancellation and rollback
// have joined. Other startup failures can use blockManagedOllamaFacade above.
func (b *Broker) blockManagedOllamaFacadePending(reason string, excludedPorts ...int) int {
	b.managedOllamaFacade.Store(false)
	b.managedOllamaBackend.Store(0)
	if backend := int(b.ollamaBackendPort.Load()); backend > 0 {
		excludedPorts = append(excludedPorts, backend)
	}
	fallback := b.setOllamaProxyFallback(excludedPorts...)
	b.reportPortOwnershipBlocked(reason)
	return fallback
}

func (b *Broker) markOllamaPortReady() {
	if b.ollamaPortReady != nil {
		b.ollamaPortReadyOnce.Do(func() { close(b.ollamaPortReady) })
	}
}

func (b *Broker) setOllamaProxyFallback(excludedPorts ...int) int {
	if aliasPort := b.currentOllamaHostAlias().Port; aliasPort > 0 {
		excludedPorts = append(excludedPorts, aliasPort)
	}
	fallback := nextAvailablePortExcluding(managedOllamaBackendStart, excludedPorts, tcpPortAvailable)
	b.ollamaProxyStartupPort.Store(int32(fallback))
	return fallback
}

// prepareManagedOllamaFacade runs before the proxy is spawned. It only builds
// a plan; the stopped backend is not persisted onto its new port until a proxy
// ready event proves :11434 has actually been reserved. Adopted/external
// engines are never moved because this slice has no process-takeover authority.
func (b *Broker) prepareManagedOllamaFacade() {
	b.prepareManagedOllamaFacadeWithPortCheck(tcpPortAvailable)
}

func (b *Broker) prepareManagedOllamaFacadeWithPortCheck(portAvailable func(int) bool) {
	finish := b.beginPortOperation("ollama")
	defer finish()
	if b.prepareExplicitEngineSettings("ollama") {
		b.markOllamaPortReady()
		return
	}
	b.setOllamaHostAlias(ollamaHostAlias{})
	// Resolve the worker inside the deferred method: engine-manager can respawn
	// while the blocking settings/status calls below are preparing the alias.
	defer b.syncCurrentEngineOllamaHostAliasReservation()

	settings := b.getSettings()
	if settings == nil {
		b.cacheOllamaPortStatus()
		b.blockManagedOllamaFacade("managed-port policy is unavailable")
		return
	}
	result, rpcErr, err := settings.Call(context.Background(), "settings/get-force-ports", nil)
	if err != nil || rpcErr != nil {
		slog.Warn("failed to read managed-port setting", "err", err, "rpcErr", rpcErr)
		b.cacheOllamaPortStatus()
		b.blockManagedOllamaFacade("managed-port policy could not be verified")
		return
	}
	var policy struct {
		Value bool `json:"value"`
	}
	if err := json.Unmarshal(result, &policy); err != nil {
		b.cacheOllamaPortStatus()
		b.blockManagedOllamaFacade("managed-port policy could not be decoded")
		return
	}

	em := b.getEngineMgr()
	if em == nil {
		if !policy.Value {
			b.setOllamaProxyFallback()
			b.forwardErrorsClear(portOwnershipBlockedID)
			b.markOllamaPortReady()
			return
		}
		b.blockManagedOllamaFacade("engine manager is unavailable")
		return
	}
	params, _ := json.Marshal(map[string]string{"engine": "ollama"})
	result, rpcErr, err = em.Call(context.Background(), "engine:status", params)
	if err != nil || rpcErr != nil {
		if !policy.Value {
			b.setOllamaProxyFallback()
			b.forwardErrorsClear(portOwnershipBlockedID)
			b.markOllamaPortReady()
			return
		}
		b.blockManagedOllamaFacade("Ollama status could not be verified")
		return
	}
	var st ollamaPortStatus
	if err := json.Unmarshal(result, &st); err != nil {
		if !policy.Value {
			b.setOllamaProxyFallback()
			b.forwardErrorsClear(portOwnershipBlockedID)
			b.markOllamaPortReady()
			return
		}
		b.blockManagedOllamaFacade("Ollama status could not be decoded")
		return
	}
	b.ollamaBackendPort.Store(int32(st.Port))
	b.prepareOllamaHostAlias(policy.Value, st.Port)
	if !policy.Value {
		// Opting out after a managed run commonly leaves stopped Ollama on
		// :11435. Start the proxy explicitly elsewhere so it cannot squat that
		// configured backend port before Ollama starts.
		b.managedOllamaFacade.Store(false)
		b.managedOllamaBackend.Store(0)
		b.setOllamaProxyFallback(st.Port)
		b.forwardErrorsClear(portOwnershipBlockedID)
		b.markOllamaPortReady()
		return
	}
	plan := planManagedOllamaPorts(true, st, b.availableOffOllamaHostAlias(portAvailable))
	if plan.Blocked != "" {
		b.blockManagedOllamaFacade(plan.Blocked, st.Port)
		return
	}
	b.managedOllamaFacade.Store(plan.Enabled)
	b.managedOllamaBackend.Store(int32(plan.BackendPort))
	b.ollamaProxyStartupPort.Store(managedOllamaFacadePort)
	if plan.BackendPort == 0 {
		slog.Info("preserving custom Ollama backend port", "port", st.Port)
		b.markOllamaPortReady()
	}
	b.forwardErrorsClear(portOwnershipBlockedID)
}

// cacheOllamaPortStatus best-effort remembers the configured backend port for
// fallback exclusion when the settings policy itself cannot be read.
func (b *Broker) cacheOllamaPortStatus() {
	em := b.getEngineMgr()
	if em == nil {
		return
	}
	params, _ := json.Marshal(map[string]string{"engine": "ollama"})
	result, rpcErr, err := em.Call(context.Background(), "engine:status", params)
	if err != nil || rpcErr != nil {
		return
	}
	var st ollamaPortStatus
	if json.Unmarshal(result, &st) == nil && st.Port > 0 {
		b.ollamaBackendPort.Store(int32(st.Port))
	}
}

func enginePortAssignmentRequest(method string, params json.RawMessage) (string, int, bool) {
	var request struct {
		Engine string `json:"engine"`
		Port   int    `json:"port"`
		Start  bool   `json:"start"`
	}
	if json.Unmarshal(params, &request) != nil || request.Engine == "" || request.Port <= 0 {
		return "", 0, false
	}
	switch method {
	case "engine:set-port", "engine:start":
		return request.Engine, request.Port, true
	case "engine:install":
		return request.Engine, request.Port, request.Start
	default:
		return "", 0, false
	}
}

func (b *Broker) rejectOllamaHostAliasPort(msg *Message, port int, owner string) bool {
	aliasPort := b.currentOllamaHostAlias().Port
	if aliasPort <= 0 || port != aliasPort {
		return false
	}
	if err := b.codec.RespondError(msg.ID, -32000, fmt.Sprintf(
		"port %d is configured as the inherited OLLAMA_HOST proxy alias; change or unset OLLAMA_HOST and restart NVPAIR before assigning that port to %s",
		port, owner)); err != nil {
		log.Printf("failed to reject port assignment conflicting with OLLAMA_HOST alias: %v", err)
	}
	return true
}

// needsOllamaPortGate reports the client requests that can probe and adopt the
// configured Ollama port. While :11434 is changing from backend to facade,
// those probes must wait or they can mistake the proxy for a local engine.
func needsOllamaPortGate(method string, params json.RawMessage) bool {
	if method == "engine:get-installed" {
		return true
	}
	if method != "engine:status" && method != "engine:install" && method != "engine:start" && method != "engine:restart" {
		return false
	}
	var request struct {
		Engine string `json:"engine"`
	}
	return json.Unmarshal(params, &request) == nil && request.Engine == "ollama"
}

// reconcileProxyPortOnReady runs after the proxy announces a bound port
// (notably its restored port on startup). If a running engine now holds that
// port, it steers the proxy to a free one — engines take precedence — and
// surfaces the move. It must run on its own goroutine (never the proxy reader
// goroutine that calls forwardProxyNotification), so the p.Call round-trip
// can't deadlock the very reader that would deliver its response.
func (b *Broker) reconcileProxyPortOnReady(boundPort int) {
	finish := b.beginPortOperation("ollama")
	defer finish()
	b.reconcileProxyPortOnReadyInOperation(boundPort)
}

func (b *Broker) reconcileProxyPortOnReadyInOperation(boundPort int) {
	b.engineConfigMu.Lock()
	retry := false
	defer func() {
		b.engineConfigMu.Unlock()
		if retry {
			// The owner releases ollamaMoveInFlight before this deferred retry.
			if p := b.getProxy(); p != nil {
				if ready, port := p.Status(); ready {
					boundPort = port
				}
			}
			go b.reconcileCurrentOllamaPorts(boundPort)
		}
	}()
	call := func(fn func()) bool {
		if b.ollamaPortIOUnlockedLocked(fn) {
			return true
		}
		retry = true
		return false
	}
	if b.loadEngineSettingsLocked() != nil {
		b.markOllamaPortReady()
		return
	}
	if _, explicit := b.explicitEngineSettingsLocked("ollama"); explicit {
		b.markOllamaPortReady()
		return
	}
	if b.managedOllamaFacade.Load() {
		p := b.getProxy()
		if p == nil {
			return
		}
		if boundPort != managedOllamaFacadePort {
			body, _ := json.Marshal(map[string]int{"port": managedOllamaFacadePort})
			var rpcErr *RPCError
			var err error
			if !call(func() { _, rpcErr, err = writeProxyPort(context.Background(), p, body) }) {
				return
			}
			if err != nil || rpcErr != nil {
				b.blockManagedOllamaFacade("the proxy could not bind the compatibility port")
				slog.Warn("failed to bind managed Ollama facade", "err", err, "rpcErr", rpcErr)
			}
			return
		}

		// Commit the pending stopped-engine move only after :11434 is ours.
		if backend := b.takePendingManagedOllamaBackend(boundPort); backend != 0 {
			releaseMove := true
			defer func() {
				if releaseMove {
					b.ollamaMoveInFlight.Store(false)
				}
			}()
			em := b.getEngineMgr()
			sourcePort := b.ollamaBackendSourcePort()
			var rpcErr *RPCError
			var err error
			fenced := true
			state := b.ollamaPortReconcileStateLocked()
			if em == nil {
				err = fmt.Errorf("engine manager unavailable")
			} else {
				current := call(func() { rpcErr, err, fenced = b.moveManagedOllamaPort(context.Background(), em, backend, state) })
				if !fenced {
					retry, releaseMove = false, false
					b.reportPortOwnershipBlocked("the backend move could not be canceled; ownership is still reconciling")
					return
				}
				if !current {
					return
				}
			}
			if err != nil || rpcErr != nil {
				// The RPC may have applied and then lost its response. Keep the
				// fallback off both candidate ports, vacate :11434 first, then
				// best-effort restore the original stopped-engine configuration.
				fallback := b.blockManagedOllamaFacadePending("the stopped Ollama backend could not be moved", backend)
				vacated := false
				if fallback != 0 {
					body, _ := json.Marshal(map[string]int{"port": fallback})
					var moveErr *RPCError
					var callErr error
					if !call(func() { _, moveErr, callErr = writeProxyPort(context.Background(), p, body) }) {
						return
					}
					if callErr != nil || moveErr != nil {
						slog.Warn("failed to vacate managed Ollama facade", "err", callErr, "rpcErr", moveErr)
					} else {
						vacated = true
					}
				}
				if vacated && em != nil {
					var rollbackErr *RPCError
					var callErr error
					state = b.ollamaPortReconcileStateLocked()
					current := call(func() {
						rollbackErr, callErr, fenced = b.moveManagedOllamaPort(context.Background(), em, sourcePort, state)
					})
					if !fenced {
						retry, releaseMove = false, false
						b.reportPortOwnershipBlocked("the backend rollback could not be canceled; ownership is still reconciling")
						return
					}
					if !current {
						return
					}
					if callErr != nil || rollbackErr != nil {
						slog.Warn("failed to restore original Ollama backend port", "err", callErr, "rpcErr", rollbackErr)
					}
				}
				b.markOllamaPortReady()
				return
			}
			b.managedOllamaBackend.Store(0)
			b.ollamaBackendPort.Store(int32(backend))
			slog.Info("configured managed Ollama backend port", "port", backend)
			b.ollamaMoveInFlight.Store(false)
		}
		if b.ollamaMoveInFlight.Load() {
			return
		}
		b.forwardErrorsClear(portOwnershipBlockedID)
		b.markOllamaPortReady()
		return
	}
	var taken map[int]bool
	if !call(func() { taken = b.runningEnginePorts() }) {
		return
	}
	if aliasPort := b.currentOllamaHostAlias().Port; aliasPort > 0 {
		taken[aliasPort] = true
	}
	if !taken[boundPort] {
		return
	}
	effective := nextFreeProxyPort(boundPort, taken)
	if effective == boundPort {
		return
	}
	p := b.getProxy()
	if p == nil {
		return
	}
	b.forwardErrorsReport(errors.ServiceError{
		ID:        proxyPortBumpedID,
		Message:   fmt.Sprintf("Port %d is in use by a running engine; the proxy was moved to %d.", boundPort, effective),
		Timestamp: nowMillis(),
		NodeID:    b.nodeID,
		Severity:  "warning",
		Action:    "none",
	})
	b.settingsStartupPort("ollama", effective)
	body, err := json.Marshal(map[string]int{"port": effective})
	if err != nil {
		slog.Warn("failed to encode corrective proxy set-port", "err", err)
		return
	}
	var rpcErr *RPCError
	var callErr error
	if !call(func() { _, rpcErr, callErr = writeProxyPort(context.Background(), p, body) }) {
		return
	}
	if callErr != nil || rpcErr != nil {
		slog.Warn("failed to reconcile proxy port on ready", "err", callErr, "rpcErr", rpcErr)
	}
}

func (b *Broker) takePendingManagedOllamaBackend(boundPort int) int {
	if !b.managedOllamaFacade.Load() || boundPort != managedOllamaFacadePort {
		return 0
	}
	if !b.ollamaMoveInFlight.CompareAndSwap(false, true) {
		return 0
	}
	backend := int(b.managedOllamaBackend.Swap(0))
	if backend == 0 {
		b.ollamaMoveInFlight.Store(false)
	}
	return backend
}

func (b *Broker) ollamaBackendSourcePort() int {
	if port := int(b.ollamaBackendPort.Load()); port > 0 {
		return port
	}
	return managedOllamaFacadePort
}

// runningEnginePorts asks nvpair-engine-manager which ports its running engines
// hold, so the proxy can be steered clear of them. A missing/slow
// engine-manager yields an empty set (no bump) — safe, because a port with no
// live listener can't actually collide at bind time; only a running engine is
// a real conflict.
func (b *Broker) runningEnginePorts() map[int]bool {
	ports := map[int]bool{}
	em := b.getEngineMgr()
	if em == nil {
		return ports
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcWorkerCallTimeout)
	defer cancel()
	result, rpcErr, err := em.Call(ctx, "engine:get-installed", nil)
	if err != nil || rpcErr != nil {
		slog.Warn("engine port lookup failed; skipping proxy port conflict check", "err", err, "rpcErr", rpcErr)
		return ports
	}
	var resp struct {
		Engines []struct {
			Running bool `json:"running"`
			Port    int  `json:"port"`
		} `json:"engines"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		slog.Warn("engine port lookup returned unparseable result", "err", err)
		return ports
	}
	for _, e := range resp.Engines {
		if e.Running && e.Port > 0 {
			ports[e.Port] = true
		}
	}
	return ports
}

// nextFreeProxyPort returns requested when it's free, otherwise the lowest
// port above it not in taken (capped at 65535).
func nextFreeProxyPort(requested int, taken map[int]bool) int {
	p := requested
	for p < 65535 && taken[p] {
		p++
	}
	return p
}
