// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"runtime"
	"testing"
	"time"

	settings "nvpair-shared/enginesettings"
)

func TestSettingsRebindOnlySelectedProxy(t *testing.T) {
	for _, engine := range []string{"ollama", "lmstudio", "omlx"} {
		t.Run(engine, func(t *testing.T) {
			h := newSettingsHarness(t)
			before := map[string]int{}
			for _, name := range []string{"ollama", "lmstudio", "omlx"} {
				_, before[name] = h.b.settingsProxy(name).Status()
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := listener.Addr().(*net.TCPAddr).Port
			_ = listener.Close()
			if err := h.b.rebindSettingsProxy(engine, port); err != nil {
				t.Fatal(err)
			}
			for name, want := range before {
				if name == engine {
					want = port
				}
				ready, got := h.b.settingsProxy(name).Status()
				if !ready || got != want {
					t.Fatalf("proxy %s: ready=%v port=%d want=%d", name, ready, got, want)
				}
			}
		})
	}
}

func TestSettingsReservesAllThreeProxies(t *testing.T) {
	for _, engine := range []string{"lmstudio", "omlx"} {
		t.Run(engine, func(t *testing.T) {
			h := newSettingsHarness(t)
			request := h.request(t)
			_, port := h.b.settingsProxy(engine).Status()
			request.Settings.ProxyPort = port
			preview, err := h.b.previewEngineSettings(context.Background(), request, "")
			if err != nil || preview.Errors["ports"] == "" {
				t.Fatalf("other proxy port accepted: %v %v", preview.Errors, err)
			}
			h.b.engineSettings[engine] = &engineSettingsRecord{Explicit: true, Snapshot: settings.Snapshot{Settings: settings.Config{ProxyPort: port}}}
			if engine == "omlx" {
				h.b.setOMLXProxy(nil)
			} else {
				h.b.setLMStudioProxy(nil)
			}
			preview, err = h.b.previewEngineSettings(context.Background(), request, "")
			if err != nil || preview.Errors["ports"] == "" {
				t.Fatal("stopped proxy reservation ignored")
			}
		})
	}
}

func TestEngineSettingsRelayDemultiplexesPermissionAndRebind(t *testing.T) {
	h := newSettingsHarness(t)
	engine, replies := newTestRPCWorkerPipe(t)
	h.b.setEngineMgr(engine)
	nodeSettings, requests := newTestRPCWorkerPipe(t)
	h.b.setSettings(nodeSettings)
	go func() {
		request, err := requests.Read()
		if err != nil {
			return
		}
		if request.Method != "settings/get-engine-allow-lan-bind" {
			t.Error("wrong permission method")
		}
		_ = requests.Respond(request.ID, map[string]bool{"value": true})
	}()
	_, port := h.b.getOMLXProxy().Status()
	h.b.activeSettings = map[string]activeSettingsOperation{"operation": {Engine: "omlx", Port: port}}
	for _, request := range []settings.Relay{
		{ID: "permission", Method: "settings/get-engine-allow-lan-bind"},
		{ID: "proxy", Method: "rebind", Request: settings.Request{Engine: "omlx", RequestID: "operation", Settings: settings.Config{ProxyPort: port}}},
	} {
		h.b.forwardEngineNotification("engine:settings-request", settingsJSON(request))
	}
	done := make(chan error, 1)
	go func() {
		seen := map[string]bool{}
		for i := 0; i < 2; i++ {
			msg, err := replies.Read()
			if err != nil {
				done <- err
				return
			}
			var reply struct {
				ID     string
				Result json.RawMessage
				Error  string
			}
			if msg.Method != "engine:settings-reply" || json.Unmarshal(msg.Params, &reply) != nil || reply.Error != "" {
				t.Error("invalid settings reply")
			}
			if reply.ID == "permission" && string(reply.Result) != `{"value":true}` {
				t.Error("permission result lost")
			}
			seen[reply.ID] = true
		}
		if !seen["permission"] || !seen["proxy"] {
			t.Error("reply correlation crossed methods")
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("settings relay did not complete both methods")
	}
}

func TestSettingsJournalPrivateAndRejectsUnpairedCaller(t *testing.T) {
	h := newSettingsHarness(t)
	request := h.request(t)
	if runtime.GOOS != "windows" {
		path, err := h.b.engineSettingsPath()
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("settings journal permissions")
		}
	}
	if _, err := h.b.getEngineSettings(context.Background(), request, "unpaired"); err == nil {
		t.Fatal("unpaired settings read accepted")
	}
	if _, err := h.b.previewEngineSettings(context.Background(), request, "unpaired"); err == nil {
		t.Fatal("unpaired settings preview accepted")
	}
	if _, err := h.b.applyEngineSettings(context.Background(), request, "unpaired"); err == nil {
		t.Fatal("unpaired settings apply accepted")
	}
	if h.applies.Load() != 0 {
		t.Fatal("unauthorized call mutated runtime")
	}
}
