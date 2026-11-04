// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"nvpair-shared/clustertrust"
	settings "nvpair-shared/enginesettings"
	"nvpair-shared/noderec"
)

func TestManagedSettingsRejectBeforeAnyRuntimeMutation(t *testing.T) {
	for _, command := range []bool{false, true} {
		e := settingsExecutor(t, command)
		inputs := []string{"OLLAMA_HOST=127.0.0.1:11434", "OLLAMA_HOST=0.0.0.0:9999"}
		if command {
			inputs = []string{"--port 1234", "--port=1234", "-p1234", "-p=1234", "-vp1234", "-- --port 1234", "--host 127.0.0.1", "--bind=0.0.0.0", "LMS_SERVER_HOST=127.0.0.1", "LMS_SERVER_PORT=1234"}
		}
		for _, input := range inputs {
			p := settingsRequest(t, e)
			p.Settings.LaunchText = input
			called := false
			_, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: p.Settings}, func() error { called = true; return nil })
			if err == nil || !strings.Contains(err.Error(), "PAIR owns") || called {
				t.Fatal("managed override reached runtime")
			}
			assertNoSettingsOverride(t, e)
		}
	}
}

func TestOMLXOnlyProxySettingsForRunningApp(t *testing.T) {
	manifest := testEngineManifest(fakeEngineBin)
	manifest.Engine = "omlx"
	for key, platform := range manifest.Platforms {
		platform.Runtime = Runtime{Mode: "command", CLI: fakeEngineBin, Port: 1235, Start: [][]string{{"{cli}", "start"}}}
		manifest.Platforms[key] = platform
	}
	e := newTestExecutor(t, manifest)
	e.overrideDir = t.TempDir()
	st, err := e.state("omlx")
	if err != nil {
		t.Fatal(err)
	}
	st.running, st.adopted = true, true
	st.port = 1235
	for _, text := range []string{"--custom-argument", "PRIVATE_SETTING=secret"} {
		config := settings.Config{ServerPort: 1235, ProxyPort: 24000, LaunchText: text}
		preview, err := e.PreviewLaunch(settings.Request{Engine: "omlx", Settings: config})
		if err != nil || !strings.Contains(preview.Errors["launchText"], "oMLX app") {
			t.Fatal("oMLX launch edit not clearly rejected")
		}
		_, err = e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "omlx", Settings: config}, func() error { t.Error("invalid oMLX settings rebound"); return nil })
		if err == nil || !strings.Contains(err.Error(), "oMLX app") {
			t.Fatal("oMLX configure accepted launch edit")
		}
	}
	if err := e.StartWith(context.Background(), "omlx", startOpts{Port: 9999}); err == nil {
		t.Fatal("oMLX start port override accepted")
	}
	if _, err := e.SetPort(context.Background(), "omlx", 9999); err == nil || !strings.Contains(err.Error(), "oMLX app") {
		t.Fatal("oMLX direct port edit accepted")
	}
	rebound := 0
	result, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "omlx", Settings: settings.Config{ServerPort: 1235, ProxyPort: 24000, LaunchText: "  "}, Resume: true}, func() error { rebound++; return nil })
	if err != nil || rebound != 1 || !result.Running || !result.Adopted || result.ServerPort != 1235 {
		t.Fatalf("proxy-only operation touched running app: %+v %v", result, err)
	}
	entries, err := os.ReadDir(e.overrideDir)
	if err != nil || len(entries) != 0 {
		t.Fatal("oMLX operation persisted launch overrides")
	}
}

func TestRemoteSettingsRequiresVerifiedInventory(t *testing.T) {
	for _, verification := range []string{"unverified", "manual", ""} {
		t.Run(verification, func(t *testing.T) {
			exec := settingsExecutor(t, false)
			var out bytes.Buffer
			manager := NewManager(NewCodec(&out), exec, clustertrust.Open(t.TempDir()))
			manager.peers.set([]noderec.DirectoryNode{{HostUUID: "claimed", ClusterUUID: "claimed-cluster", IP: "127.0.0.1", Trusted: true, PeerRecordID: "preview:claimed", IdentityVerification: verification, Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceEngineControl: {Port: 14323}}}})
			for _, method := range []string{"engine:remote-get-settings", "engine:remote-preview-settings", "engine:remote-apply-settings"} {
				out.Reset()
				id := json.RawMessage("1")
				manager.remoteSettings(context.Background(), &Message{ID: &id, Method: method, Params: json.RawMessage(`{"nodeId":"claimed","engine":"ollama"}`)})
				if !strings.Contains(out.String(), "settings unavailable") {
					t.Fatal("unverified inventory reached remote settings transport")
				}
			}
		})
	}
}
