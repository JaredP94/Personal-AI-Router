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

// llamacppproxy.go is the broker's llama.cpp counterpart to its ollama-proxy and
// lmstudio-proxy wiring. llamacpp-proxy speaks the exact same JSON-RPC control plane
// as ollama-proxy/lmstudio-proxy (a "ready" port notification,
// node/add-manual/remove-manual, nodes/list, node/select, the workload:* lifecycle
// stream), so it reuses the proxyProcess client type; only the namespace differs
// — the broker relays it under llamacpp-proxy: instead of proxy: or lmstudio-proxy:.

func (b *Broker) setLlamaCPPProxy(p *proxyProcess) {
	b.workersMu.Lock()
	b.llamacppProxy = p
	b.workersMu.Unlock()
}

func (b *Broker) getLlamaCPPProxy() *proxyProcess {
	b.workersMu.Lock()
	defer b.workersMu.Unlock()
	return b.llamacppProxy
}

func (b *Broker) llamacppProxyListenPort() int {
	p := b.getLlamaCPPProxy()
	if p == nil {
		return 0
	}
	ready, port := p.Status()
	if !ready {
		return 0
	}
	return port
}

func (b *Broker) configureLlamaCPPProxySupervisorCallbacks(sup *supervisor) {
	sup.onCrash, sup.onRecovered = b.supervisedWorkerCallbacks("llamacpp-proxy", func() { b.setLlamaCPPProxy(nil) })
	sup.onExhausted = func(attempt int) {
		slog.Warn("llamacpp-proxy is terminally unavailable", "attempt", attempt)
	}
}

func (b *Broker) llamacppProxyArgs() []string {
	var args []string
	if port := int(b.llamacppProxyStartupPort.Load()); port != 0 {
		args = []string{"--port", fmt.Sprintf("%d", port), "--ignore-persisted-port"}
	}
	return append(args, b.clusterDirArgs()...)
}

func (b *Broker) spawnLlamaCPPProxy() (supervisedHandle, error) {
	finish := b.beginPortOperation("llamacpp")
	defer finish()
	generation := b.llamacppProxyGeneration.Add(1)
	pp, err := startProxy(
		"llamacpp-proxy",
		b.llamacppProxyPath,
		applog.LevelString(),
		b.relayDir,
		func(method string, params json.RawMessage) {
			b.forwardLlamaCPPProxyNotificationForGeneration(generation, method, params)
		},
		b.llamacppProxyArgs()...,
	)
	if err != nil {
		return nil, err
	}
	b.setLlamaCPPProxy(pp)
	if ready, _ := pp.Status(); ready {
		go b.reconcileTelemetry()
	}
	b.llamacppProxyPublishedGeneration.Store(generation)
	if ready, port := pp.Status(); ready && port > 0 {
		go b.reconcileLlamaCPPProxyPortOnReadyForGeneration(generation, port)
	}
	slog.Info("llamacpp-proxy started", "path", b.llamacppProxyPath, "pid", pp.cmd.Process.Pid)
	return pp, nil
}

func (b *Broker) forwardLlamaCPPProxyNotification(method string, params json.RawMessage) {
	b.forwardLlamaCPPProxyNotificationForGeneration(b.llamacppProxyGeneration.Load(), method, params)
}

func (b *Broker) forwardLlamaCPPProxyNotificationForGeneration(generation uint64, method string, params json.RawMessage) {
	if b.llamacppProxyGeneration.Load() != generation {
		return
	}
	if b.dispatchErrorsNotif("llamacpp-proxy", method, params) {
		return
	}
	if method == "error" {
		var ep struct {
			Code string `json:"code"`
			Port int    `json:"port"`
		}
		if json.Unmarshal(params, &ep) == nil && ep.Code == "bind-failed" && !b.llamacppExplicitSettings.Load() {
			go b.recoverProxyBindFailure("llamacpp", generation, ep.Port)
		}
	}
	if b.routeProxyWorkloadMethod(method, params) {
		return
	}
	if method == noderec.NotifyNodeActivity {
		b.routeNodeActivity(params)
		return
	}
	if method == "ready" {
		go b.reconcileTelemetry()
		var rp proxyReadyParams
		if err := json.Unmarshal(params, &rp); err == nil && rp.Port > 0 {
			go b.reconcileLlamaCPPProxyPortOnReadyForGeneration(generation, rp.Port)
		}
	}
	b.proxyMu.Lock()
	subscribed := b.llamacppProxySubscribed
	b.proxyMu.Unlock()
	if !subscribed {
		return
	}
	if err := b.codec.Notify("llamacpp-proxy:"+method, params); err != nil {
		slog.Warn("forward llamacpp-proxy notification failed", "method", method, "err", err)
	}
}

func (b *Broker) reconcileLlamaCPPProxyPortOnReadyForGeneration(generation uint64, boundPort int) {
	finish := b.beginPortOperation("llamacpp")
	defer finish()
	if b.llamacppProxyGeneration.Load() != generation {
		return
	}
	b.repushPriority("llamacpp")
	b.reconcileAdvertiseLlamaCPP(http.DefaultClient)
}

// Prepare accepted settings before spawning; implicit defaults may move to an
// unoccupied port, while explicit settings never silently change.
func (b *Broker) prepareLlamaCPPProxyPort() {
	finish := b.beginPortOperation("llamacpp")
	defer finish()
	if b.prepareExplicitEngineSettings("llamacpp") || b.llamacppProxyStartupPort.Load() != 0 {
		return
	}
	b.setLlamaCPPProxyFallback()
}

func (b *Broker) setLlamaCPPProxyFallback(excluded ...int) int {
	port := b.nextProxyFallbackPort("llamacpp", 1237, excluded, tcpPortAvailable)
	b.llamacppProxyStartupPort.Store(int32(port))
	return port
}

func (b *Broker) relayToLlamaCPPProxy(msg *Message) {
	method := strings.TrimPrefix(msg.Method, "llamacpp-proxy:")
	if method == "shutdown" {
		if err := b.codec.RespondError(msg.ID, -32601, "llamacpp-proxy:shutdown is not allowed; the broker owns the proxy lifecycle"); err != nil {
			log.Printf("failed to respond to llamacpp-proxy:shutdown: %v", err)
		}
		return
	}

	p := b.getLlamaCPPProxy()
	if p == nil {
		if err := b.codec.RespondError(msg.ID, -32000, "llamacpp-proxy not available"); err != nil {
			log.Printf("failed to respond to %s: %v", msg.Method, err)
		}
		return
	}

	result, rpcErr, err := p.Call(context.Background(), method, msg.Params)
	switch {
	case err != nil:
		if err := b.codec.RespondError(msg.ID, -32000, fmt.Sprintf("llamacpp-proxy call failed: %v", err)); err != nil {
			log.Printf("failed to respond to %s: %v", msg.Method, err)
		}
	case rpcErr != nil:
		if err := b.codec.RespondError(msg.ID, rpcErr.Code, rpcErr.Message); err != nil {
			log.Printf("failed to relay llamacpp-proxy error for %s: %v", msg.Method, err)
		}
	default:
		if err := b.codec.Respond(msg.ID, result); err != nil {
			log.Printf("failed to relay llamacpp-proxy result for %s: %v", msg.Method, err)
		}
	}
}
