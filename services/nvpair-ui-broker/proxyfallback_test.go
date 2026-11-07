// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"nvpair-shared/appdir"
	settings "nvpair-shared/enginesettings"
	"nvpair-shared/noderec"
	"nvpair-ui-broker/relay"
)

func TestProxyFallbackAllowsOwnDefaultAndSavedPorts(t *testing.T) {
	isolateOllamaHostTestConfig(t)
	for _, tc := range []struct {
		engine string
		port   int
	}{
		{"ollama", 11434}, {"lmstudio", 1234}, {"omlx", 1236}, {"llamacpp", 1237},
	} {
		t.Run(tc.engine, func(t *testing.T) {
			b := &Broker{engineSettings: map[string]*engineSettingsRecord{
				tc.engine: {Snapshot: settings.Snapshot{Settings: settings.Config{ServerPort: 12410, ProxyPort: tc.port}}, Previous: settings.Config{ServerPort: 12412, ProxyPort: 12413}},
			}, regCache: relay.NewRegistrationCache()}
			b.settingsStartupPort(tc.engine, 12414)
			b.regCache.Register(noderec.RegisterParams{Service: engineSettingsService(tc.engine), Port: 12415})
			// A generation can still report its previous port during recovery.
			proxy := &proxyProcess{ready: true, port: 12416}
			switch tc.engine {
			case "ollama":
				b.setProxy(proxy)
			case "lmstudio":
				b.setLMStudioProxy(proxy)
			case "omlx":
				b.setOMLXProxy(proxy)
			case "llamacpp":
				b.setLlamaCPPProxy(proxy)
			}
			ports := b.proxyFallbackExclusions(tc.engine, []int{12417})
			for _, own := range []int{tc.port, 12413, 12414, 12415, 12416} {
				if slices.Contains(ports, own) {
					t.Errorf("own port %d excluded: %v", own, ports)
				}
				if got := b.nextProxyFallbackPort(tc.engine, own, nil, func(int) bool { return true }); got != own {
					t.Errorf("selected %d, want own port %d", got, own)
				}
			}
			for _, reserved := range []int{11435, 1235, 8082, 12410, 12412, 12417} {
				if !slices.Contains(ports, reserved) {
					t.Errorf("reserved port %d allowed", reserved)
				}
			}
			for _, other := range []int{11434, 1234, 1236, 1237} {
				if other != tc.port && !slices.Contains(ports, other) {
					t.Errorf("other proxy default %d allowed", other)
				}
			}
			if got := b.nextProxyFallbackPort(tc.engine, tc.port, []int{tc.port}, func(int) bool { return true }); got == tc.port {
				t.Fatal("caller exclusion ignored for own port")
			}
		})
	}
}

func TestProxyFallbackPrefersPersistedPort(t *testing.T) {
	for engine, file := range map[string]string{
		"ollama": "proxy-port.json", "lmstudio": "lmstudio-proxy-port.json",
		"omlx": "omlx-proxy-port.json", "llamacpp": "llamacpp-proxy-port.json",
	} {
		t.Run(engine, func(t *testing.T) {
			isolateOllamaHostTestConfig(t)
			path, err := appdir.Path(file)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(`{"port":12420}`), 0600); err != nil {
				t.Fatal(err)
			}
			b := &Broker{}
			free := func(int) bool { return true }
			if got := b.nextProxyFallbackPort(engine, 1236, nil, free); got != 12420 {
				t.Fatalf("discarded free saved port: got %d, want 12420", got)
			}
			if got := b.nextProxyFallbackPort(engine, 1236, []int{12420}, free); got == 12420 {
				t.Fatal("bind-failure exclusion ignored for saved port")
			}
			if got := b.nextProxyFallbackPort(engine, 1236, nil, func(port int) bool { return port != 12420 }); got == 12420 {
				t.Fatal("occupied saved port reused")
			}
			if engine == "omlx" {
				b.lmstudioProxyPath = "/fixture/lmstudio-proxy"
				b.prepareOMLXProxyPortWithPortCheck(free)
				if got := int(b.omlxProxyStartupPort.Load()); got != 12420 {
					t.Fatalf("oMLX startup port = %d", got)
				}
			}
			if engine == "llamacpp" {
				b.prepareLlamaCPPProxyPort()
				if got := int(b.llamacppProxyStartupPort.Load()); got != 12420 {
					t.Fatalf("llama.cpp startup port = %d", got)
				}
			}
		})
	}
}

func TestOMLXStartupPreservesSavedPortAndRevision(t *testing.T) {
	isolateOllamaHostTestConfig(t)
	h := newSettingsHarness(t)
	b := h.b
	proxy := b.getOMLXProxy()
	proxy.port = 1236
	before, err := b.getEngineSettings(context.Background(), settings.Request{Engine: "omlx"}, "")
	if err != nil {
		t.Fatal(err)
	}
	b.setOMLXProxy(nil)
	b.getLMStudioProxy().port = 1234
	b.getLlamaCPPProxy().port = 1237
	b.lmstudioProxyPath = "/fixture/lmstudio-proxy"
	b.prepareOMLXProxyPortWithPortCheck(func(port int) bool { return port != 1234 })
	port := int(b.omlxProxyStartupPort.Load())
	if port != 1236 {
		t.Errorf("startup port = %d, want saved 1236", port)
	}
	proxy.port = port
	b.setOMLXProxy(proxy)
	after, err := b.getEngineSettings(context.Background(), settings.Request{Engine: "omlx"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision || after.Settings.ProxyPort != before.Settings.ProxyPort {
		t.Fatalf("startup changed saved settings: before %+v; after %+v", before, after)
	}
}

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
