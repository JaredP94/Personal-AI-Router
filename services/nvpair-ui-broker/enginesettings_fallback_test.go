// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"nvpair-shared/appdir"
	settings "nvpair-shared/enginesettings"
)

func saveSettingsHarnessProxyPort(t *testing.T, engine string, port int) {
	t.Helper()
	file := map[string]string{
		"ollama": "proxy-port.json", "lmstudio": "lmstudio-proxy-port.json",
		"omlx": "omlx-proxy-port.json", "llamacpp": "llamacpp-proxy-port.json",
	}[engine]
	path, err := appdir.Path(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"port":%d}`, port)), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsRefreshPreservesConfiguredProxyPortOnFallback(t *testing.T) {
	for _, engine := range []string{"ollama", "lmstudio", "omlx", "llamacpp"} {
		for _, explicit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/explicit=%t", engine, explicit), func(t *testing.T) {
				h := newSettingsHarness(t)
				b := h.b
				before, err := b.getEngineSettings(context.Background(), settings.Request{Engine: engine}, "")
				if err != nil {
					t.Fatal(err)
				}
				b.engineConfigMu.Lock()
				b.engineSettings[engine].Explicit = explicit
				if err := b.saveEngineSettingsLocked(); err != nil {
					t.Fatal(err)
				}
				b.engineConfigMu.Unlock()
				path, err := b.engineSettingsPath()
				if err != nil {
					t.Fatal(err)
				}
				journal, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				// An unrelated process holds the desired port during startup.
				conflict, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", before.Settings.ProxyPort))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = conflict.Close() })
				fallback := b.nextProxyFallbackPort(engine, 12430, nil, tcpPortAvailable)
				if fallback == 0 || fallback == before.Settings.ProxyPort {
					t.Fatalf("expected fallback from occupied port %d, got %d", before.Settings.ProxyPort, fallback)
				}
				proxy := b.settingsProxy(engine)
				proxy.readyMu.Lock()
				proxy.port = fallback
				proxy.readyMu.Unlock()
				for i := 0; i < 2; i++ {
					after, err := b.getEngineSettings(context.Background(), settings.Request{Engine: engine}, "")
					if err != nil {
						t.Fatal(err)
					}
					if after.Settings != before.Settings || after.Revision != before.Revision || after.AppliedRevision != before.AppliedRevision {
						t.Errorf("fallback changed saved configuration: before %+v; after %+v", before, after)
					}
					if after.EffectiveProxyPort != fallback {
						t.Errorf("effective proxy port = %d, want fallback %d", after.EffectiveProxyPort, fallback)
					}
				}
				persisted, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(persisted, journal) {
					t.Errorf("refresh rewrote the settings journal: %v", err)
				}
				if err := conflict.Close(); err != nil {
					t.Fatal(err)
				}
				// A new broker generation must retain the desired configuration
				// and choose that port again once the conflict has cleared.
				restarted := &Broker{clusterDir: b.clusterDir}
				restarted.engineConfigMu.Lock()
				if err := restarted.loadEngineSettingsLocked(); err != nil {
					t.Fatal(err)
				}
				restored := restarted.engineSettings[engine].Snapshot
				restarted.engineConfigMu.Unlock()
				if restored.Settings != before.Settings || restored.Revision != before.Revision {
					t.Errorf("restart lost saved configuration: %+v", restored)
				}
				if got := restarted.nextProxyFallbackPort(engine, 12430, nil, tcpPortAvailable); got != before.Settings.ProxyPort {
					t.Errorf("next startup port = %d, want saved %d", got, before.Settings.ProxyPort)
				}
			})
		}
	}
}

func TestSettingsFirstSnapshotUsesConfiguredProxyPort(t *testing.T) {
	for _, engine := range []string{"ollama", "lmstudio", "omlx", "llamacpp"} {
		for _, saved := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/saved=%t", engine, saved), func(t *testing.T) {
				h := newSettingsHarness(t)
				if !saved {
					// Remove fixture choices to exercise the broker's defaults.
					isolateOllamaHostTestConfig(t)
				}
				configured := configuredProxyPort(engine)
				proxy := h.b.settingsProxy(engine)
				proxy.readyMu.Lock()
				proxy.port = configured + 20
				proxy.readyMu.Unlock()
				snapshot, err := h.b.getEngineSettings(context.Background(), settings.Request{Engine: engine}, "")
				if err != nil {
					t.Fatal(err)
				}
				if snapshot.Settings.ProxyPort != configured || snapshot.EffectiveProxyPort != configured+20 {
					t.Fatalf("first snapshot confused configured and effective ports: %+v", snapshot)
				}
				if snapshot.Revision != 1 || snapshot.AppliedRevision != 1 || snapshot.Phase != "idle" || h.b.engineSettings[engine].Explicit {
					t.Fatalf("first record initialization changed: %+v", snapshot)
				}
			})
		}
	}
}
