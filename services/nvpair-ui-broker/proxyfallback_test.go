// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"nvpair-shared/appdir"
	settings "nvpair-shared/enginesettings"
	"nvpair-shared/noderec"
	"nvpair-ui-broker/relay"
)

// Inventory enumeration precedes a fallback bind, but never performs an engine
// status probe against the proxy's not-yet-vacated port.
func readPastFallbackInventory(codec *Codec, port int) (*Message, error) {
	for {
		msg, err := codec.Read()
		if err != nil || msg.Method != "engine:configured-ports" {
			return msg, err
		}
		if err := codec.Respond(msg.ID, map[string]any{"engines": []any{map[string]any{"engine": "lmstudio", "port": port}}}); err != nil {
			return nil, err
		}
	}
}

func TestProxyFallbackExcludesProxyAndNodePorts(t *testing.T) {
	isolateOllamaHostTestConfig(t)
	b := &Broker{regCache: relay.NewRegistrationCache(), engineSettings: map[string]*engineSettingsRecord{
		"omlx": {Snapshot: settings.Snapshot{Settings: settings.Config{ServerPort: 12401, ProxyPort: 12402}}},
	}}
	b.regCache.Register(noderec.RegisterParams{Service: noderec.ServiceNodeInfo, Port: 12403})
	b.omlxProxyStartupPort.Store(12404)
	b.setLMStudioProxy(&proxyProcess{ready: true, port: 12405})
	for _, name := range []string{"proxy-port.json", "lmstudio-proxy-port.json", "omlx-proxy-port.json"} {
		path, err := appdir.Path(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"port":12406}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ports := b.proxyFallbackExclusions("llamacpp", nil)
	for _, port := range []int{12401, 12402, 12403, 12404, 12405, 12406, engineManagerHTTPPort, engineControlPort, nodeInfoHTTPPort, errorsHTTPPort, workloadHTTPPort, clusterManagerHTTPPort} {
		if !slices.Contains(ports, port) {
			t.Errorf("port %d not excluded", port)
		}
	}
}

func TestProxyFallbackInventoryReleasesSettingsLock(t *testing.T) {
	worker, codec := newTestRPCWorkerPipe(t)
	b := &Broker{}
	b.setEngineMgr(worker)
	unlocked := make(chan bool, 1)
	go func() {
		msg, err := codec.Read()
		if err != nil {
			return
		}
		ok := b.engineConfigMu.TryLock()
		if ok {
			b.engineConfigMu.Unlock()
		}
		unlocked <- ok
		_ = codec.Respond(msg.ID, map[string]any{"engines": []any{}})
	}()
	finish := b.beginPortOperation("llamacpp")
	b.setLlamaCPPProxyFallback()
	finish()
	if !<-unlocked {
		t.Fatal("settings lock held during inventory worker call")
	}
}

func TestProxyFallbackReservesStoppedEnginePorts(t *testing.T) {
	for _, tc := range []struct {
		engine string
		port   int
		choose func(*Broker) int
	}{
		{"llamacpp", 1237, func(b *Broker) int { return b.setLlamaCPPProxyFallback() }},
		{"ollama", 11435, func(b *Broker) int { return b.setOllamaProxyFallback() }},
		{"lmstudio", 1235, func(b *Broker) int { return b.setLMStudioProxyFallback() }},
		{"omlx", 1236, func(b *Broker) int { return b.setOMLXProxyFallback() }},
	} {
		t.Run(tc.engine, func(t *testing.T) {
			// The static inventory includes every engine, including stopped ones.
			// The llama.cpp case reproduces stopped oMLX configured on 1237.
			other := "omlx"
			if tc.engine == other {
				other = "llamacpp"
			}
			b := brokerWithEngineInventory(t, map[string]int{other: tc.port, tc.engine: tc.port + 1, "custom": tc.port + 2})
			finish := b.beginPortOperation(tc.engine)
			port := tc.choose(b)
			finish()
			if port <= 0 || port == tc.port || port == tc.port+1 || port == tc.port+2 {
				t.Fatalf("fallback %d stole a stopped engine's configured port (%d..%d)", port, tc.port, tc.port+2)
			}
		})
	}
}
