// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
)

// Called under the target engine's port-operation serialiser. Snapshot local
// settings under their short lock, then release it BEFORE any worker call.
// Stopped engines reserve their configured ports just as running ones do.
func (b *Broker) proxyFallbackExclusions(engine string, excluded []int) []int {
	excluded = append(excluded, 11434, 11435, 1234, 1235, 1236, 8082,
		engineManagerHTTPPort, engineControlPort, nodeInfoHTTPPort, errorsHTTPPort, workloadHTTPPort, clusterManagerHTTPPort)
	if port := b.currentOllamaHostAlias().Port; port > 0 {
		excluded = append(excluded, port)
	}
	b.engineConfigMu.Lock()
	for _, record := range b.engineSettings {
		if record != nil {
			excluded = append(excluded, record.Snapshot.Settings.ServerPort, record.Snapshot.Settings.ProxyPort,
				record.Previous.ServerPort, record.Previous.ProxyPort)
		}
	}
	b.engineConfigMu.Unlock()
	for _, other := range []string{"ollama", "lmstudio", "omlx", "llamacpp"} {
		excluded = append(excluded, b.proxyStartupPort(other))
		if proxy := b.settingsProxy(other); proxy != nil {
			_, port := proxy.Status()
			excluded = append(excluded, port)
		}
		if other != engine {
			excluded = append(excluded, configuredProxyPort(other))
		}
	}
	if b.regCache != nil {
		for _, service := range b.regCache.Snapshot() {
			excluded = append(excluded, service.Port)
		}
	}
	if b.getEngineMgr() != nil {
		var configured struct {
			Engines []struct {
				Port int `json:"port"`
			} `json:"engines"`
		}
		ctx, cancel := context.WithTimeout(context.Background(), rpcWorkerCallTimeout)
		err := b.settingsWorkerCall(ctx, "engine:configured-ports", nil, &configured)
		cancel()
		if err != nil {
			slog.Warn("cannot verify configured engine ports for proxy fallback", "engine", engine, "err", err)
			// No safe candidate is proven. Do not probe arbitrary ports on a
			// failed inventory read; the next supervised attempt can retry.
			return nil
		}
		for _, configuredEngine := range configured.Engines {
			excluded = append(excluded, configuredEngine.Port)
		}
	}
	return excluded
}

func (b *Broker) nextProxyFallbackPort(engine string, start int, excluded []int, available func(int) bool) int {
	ports := b.proxyFallbackExclusions(engine, excluded)
	if ports == nil {
		return 0
	}
	return nextAvailablePortExcluding(start, ports, available)
}
