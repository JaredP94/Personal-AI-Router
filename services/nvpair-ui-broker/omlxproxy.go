// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"strings"

	"nvpair-shared/applog"
	"nvpair-shared/noderec"
)

// omlxproxy.go is the broker's oMLX counterpart to its ollama-proxy and
// lmstudio-proxy wiring. omlx-proxy speaks the exact same JSON-RPC control plane
// as ollama-proxy/lmstudio-proxy (a "ready" port notification,
// node/add-manual/remove-manual, nodes/list, node/select, the workload:* lifecycle
// stream), so it reuses the proxyProcess client type; only the namespace differs
// — the broker relays it under omlx-proxy: instead of proxy: or lmstudio-proxy:.

func (b *Broker) setOMLXProxy(p *proxyProcess) {
	b.workersMu.Lock()
	b.omlxProxy = p
	b.workersMu.Unlock()
}

func (b *Broker) getOMLXProxy() *proxyProcess {
	b.workersMu.Lock()
	defer b.workersMu.Unlock()
	return b.omlxProxy
}

func (b *Broker) omlxProxyListenPort() int {
	p := b.getOMLXProxy()
	if p == nil {
		return 0
	}
	ready, port := p.Status()
	if !ready {
		return 0
	}
	return port
}

func (b *Broker) configureOMLXProxySupervisorCallbacks(sup *supervisor) {
	sup.onCrash, sup.onRecovered = b.supervisedWorkerCallbacks("omlx-proxy", func() { b.setOMLXProxy(nil) })
	sup.onExhausted = func(attempt int) {
		slog.Warn("omlx-proxy is terminally unavailable", "attempt", attempt)
	}
}

func (b *Broker) omlxProxyArgs() []string {
	var args []string
	if port := int(b.omlxProxyStartupPort.Load()); port != 0 {
		args = []string{"--port", fmt.Sprintf("%d", port), "--ignore-persisted-port"}
	}
	return append(args, b.clusterDirArgs()...)
}

func (b *Broker) spawnOMLXProxy() (supervisedHandle, error) {
	generation := b.omlxProxyGeneration.Add(1)
	pp, err := startProxy(
		"omlx-proxy",
		b.omlxProxyPath,
		applog.LevelString(),
		b.relayDir,
		func(method string, params json.RawMessage) {
			b.forwardOMLXProxyNotificationForGeneration(generation, method, params)
		},
		b.omlxProxyArgs()...,
	)
	if err != nil {
		return nil, err
	}
	b.setOMLXProxy(pp)
	b.omlxProxyPublishedGeneration.Store(generation)
	if ready, port := pp.Status(); ready && port > 0 {
		go b.reconcileOMLXProxyPortOnReadyForGeneration(generation, port)
	}
	slog.Info("omlx-proxy started", "path", b.omlxProxyPath, "pid", pp.cmd.Process.Pid)
	return pp, nil
}

func (b *Broker) forwardOMLXProxyNotification(method string, params json.RawMessage) {
	b.forwardOMLXProxyNotificationForGeneration(b.omlxProxyGeneration.Load(), method, params)
}

func (b *Broker) forwardOMLXProxyNotificationForGeneration(generation uint64, method string, params json.RawMessage) {
	if b.omlxProxyGeneration.Load() != generation {
		return
	}
	if b.dispatchErrorsNotif("omlx-proxy", method, params) {
		return
	}
	if method == "error" {
		var ep struct {
			Code string `json:"code"`
			Port int    `json:"port"`
		}
		if json.Unmarshal(params, &ep) == nil && ep.Code == "bind-failed" {
			fallback := b.setOMLXProxyFallback(ep.Port)
			slog.Warn("oMLX proxy bind failed; retrying on fallback", "port", ep.Port, "fallback", fallback)
		}
	}
	if proxyWorkloadMethods[method] {
		b.routeProxyWorkload(method, params)
		return
	}
	if method == noderec.NotifyNodeActivity {
		b.routeNodeActivity(params)
		return
	}
	if method == "ready" {
		var rp proxyReadyParams
		if err := json.Unmarshal(params, &rp); err == nil && rp.Port > 0 {
			b.forwardErrorsClear(subprocessCrashedID("omlx-proxy"))
			go b.reconcileOMLXProxyPortOnReadyForGeneration(generation, rp.Port)
		}
	}
	b.proxyMu.Lock()
	subscribed := b.omlxProxySubscribed
	b.proxyMu.Unlock()
	if !subscribed {
		return
	}
	if err := b.codec.Notify("omlx-proxy:"+method, params); err != nil {
		slog.Warn("forward omlx-proxy notification failed", "method", method, "err", err)
	}
}

func (b *Broker) reconcileOMLXProxyPortOnReadyForGeneration(generation uint64, boundPort int) {
	if b.omlxProxyGeneration.Load() != generation {
		return
	}
	b.repushPriority("omlx")
	b.reconcileAdvertiseOMLX(http.DefaultClient)
}

// prepareOMLXProxyPort runs before omlx-proxy is spawned. When port 1234
// is occupied by an active listener or reserved by the broker for lmstudio-proxy,
// it assigns an alternate fallback port upfront so omlx-proxy binds cleanly
// on attempt 1 without an initial crash and recovery cycle.
func (b *Broker) prepareOMLXProxyPort() {
	b.prepareOMLXProxyPortWithPortCheck(tcpPortAvailable)
}

func (b *Broker) prepareOMLXProxyPortWithPortCheck(portAvailable func(int) bool) {
	if b.omlxProxyStartupPort.Load() != 0 {
		return
	}
	if b.omlxPort1234Occupied(portAvailable) {
		fallback := b.setOMLXProxyFallbackWithPortCheck(portAvailable, 1234)
		slog.Info("oMLX proxy port 1234 in use or reserved for LM Studio; configured upfront fallback", "fallback", fallback)
	}
}

func (b *Broker) omlxPort1234Occupied(portAvailable func(int) bool) bool {
	// If lmstudio-proxy is configured to run, it will bind to port 1234 (unless explicitly configured otherwise).
	if b.lmstudioProxyPath != "" {
		stPort := int(b.lmstudioProxyStartupPort.Load())
		if stPort == 0 || stPort == 1234 {
			return true
		}
	}
	if lmPort := b.lmstudioProxyListenPort(); lmPort == 1234 {
		return true
	}
	if b.managedLMStudioFacade.Load() {
		return true
	}
	return !portAvailable(1234)
}

func (b *Broker) setOMLXProxyFallback(excludedPorts ...int) int {
	return b.setOMLXProxyFallbackWithPortCheck(tcpPortAvailable, excludedPorts...)
}

func (b *Broker) setOMLXProxyFallbackWithPortCheck(portAvailable func(int) bool, excludedPorts ...int) int {
	if aliasPort := b.currentOllamaHostAlias().Port; aliasPort > 0 {
		excludedPorts = append(excludedPorts, aliasPort)
	}
	if lmPort := b.lmstudioProxyListenPort(); lmPort > 0 {
		excludedPorts = append(excludedPorts, lmPort)
	}
	if lmStPort := int(b.lmstudioProxyStartupPort.Load()); lmStPort > 0 {
		excludedPorts = append(excludedPorts, lmStPort)
	}
	excludedPorts = append(excludedPorts, 1234, 1235)
	fallback := nextAvailablePortExcluding(1236, excludedPorts, portAvailable)
	b.omlxProxyStartupPort.Store(int32(fallback))
	return fallback
}

func (b *Broker) relayToOMLXProxy(msg *Message) {
	method := strings.TrimPrefix(msg.Method, "omlx-proxy:")
	if method == "shutdown" {
		if err := b.codec.RespondError(msg.ID, -32601, "omlx-proxy:shutdown is not allowed; the broker owns the proxy lifecycle"); err != nil {
			log.Printf("failed to respond to omlx-proxy:shutdown: %v", err)
		}
		return
	}

	p := b.getOMLXProxy()
	if p == nil {
		if err := b.codec.RespondError(msg.ID, -32000, "omlx-proxy not available"); err != nil {
			log.Printf("failed to respond to %s: %v", msg.Method, err)
		}
		return
	}

	result, rpcErr, err := p.Call(context.Background(), method, msg.Params)
	switch {
	case err != nil:
		if err := b.codec.RespondError(msg.ID, -32000, fmt.Sprintf("omlx-proxy call failed: %v", err)); err != nil {
			log.Printf("failed to respond to %s: %v", msg.Method, err)
		}
	case rpcErr != nil:
		if err := b.codec.RespondError(msg.ID, rpcErr.Code, rpcErr.Message); err != nil {
			log.Printf("failed to relay omlx-proxy error for %s: %v", msg.Method, err)
		}
	default:
		if err := b.codec.Respond(msg.ID, result); err != nil {
			log.Printf("failed to relay omlx-proxy result for %s: %v", msg.Method, err)
		}
	}
}
