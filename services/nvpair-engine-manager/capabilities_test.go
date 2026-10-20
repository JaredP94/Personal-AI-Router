// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

// assertCapabilities checks the wire contract: every flag must be present — an
// omitted flag is indistinguishable from an older peer that reports no
// capabilities at all — and carry the expected value. Expectations are read back
// through Capabilities' own JSON tags so the flag list cannot drift.
func assertCapabilities(t *testing.T, status EngineStatus, want Capabilities) {
	t.Helper()
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	wantData, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var wantFlags map[string]bool
	if err := json.Unmarshal(wantData, &wantFlags); err != nil {
		t.Fatal(err)
	}
	if len(wire.Capabilities) != len(wantFlags) {
		t.Fatalf("capabilities must carry all %d flags, got %s", len(wantFlags), data)
	}
	for key, w := range wantFlags {
		if got, ok := wire.Capabilities[key]; !ok || got != w {
			t.Errorf("capabilities.%s = %v (present=%v), want %v", key, got, ok, w)
		}
	}
}

// TestBundledManifestCapabilities is the capability contract table for the three
// bundled manifests, per platform. It pins what every client gates on, notably
// that oMLX never offers pull or delete and never an editable port.
func TestBundledManifestCapabilities(t *testing.T) {
	base := resolutionRoot(t) // no real user CLI may affect the capability table
	reg := loadWithOverrides(t, t.TempDir())
	for _, tc := range []struct {
		engine, platform string
		adopted          bool
		want             Capabilities
	}{
		{"ollama", "darwin/arm64", false, Capabilities{
			ListModels: true, PullModel: true, DeleteModel: true,
			SetPort: true, Install: true, Uninstall: true,
		}},
		{"ollama", "windows/amd64", false, Capabilities{
			ListModels: true, PullModel: true, DeleteModel: true,
			SetPort: true, Install: true, Uninstall: true,
		}},
		{"lmstudio", "darwin/arm64", false, Capabilities{
			ListModels: true, PullModel: true, DeleteModel: true,
			// Only LM Studio declares restart_after, so its delete interrupts a
			// running engine and a caller has to warn about it.
			DeleteRestartsEngine: true,
			SetPort:              true, Install: true,
		}},
		{"lmstudio", "windows/amd64", false, Capabilities{
			ListModels: true, PullModel: true, DeleteModel: true,
			DeleteRestartsEngine: true,
			SetPort:              true, Install: true,
		}},
		// oMLX lists models and is installable, but declares no pull_model or
		// delete_model action, and its start command takes no port. Command-mode
		// uninstall requires a resolved, covered CLI; none is installed here.
		{"omlx", "darwin/arm64", false, Capabilities{
			ListModels: true, Install: true,
		}},
		// oMLX ships for macOS arm64 only: an unsupported host supports nothing.
		{"omlx", "linux/amd64", false, Capabilities{}},
		{"omlx", "windows/amd64", false, Capabilities{}},
		// An adopted Ollama is externally managed, so NVPAIR cannot move it and
		// has no managed image to uninstall.
		{"ollama", "darwin/arm64", true, Capabilities{
			ListModels: true, PullModel: true, DeleteModel: true, Install: true,
		}},
		// An adopted LM Studio still has an official stop command, so it can move.
		{"lmstudio", "darwin/arm64", true, Capabilities{
			ListModels: true, PullModel: true, DeleteModel: true,
			DeleteRestartsEngine: true,
			SetPort:              true, Install: true,
		}},
	} {
		t.Run(fmt.Sprintf("%s/%s/adopted=%t", tc.engine, tc.platform, tc.adopted), func(t *testing.T) {
			m, ok := reg.Get(tc.engine)
			if !ok {
				t.Fatalf("bundled engine %q not loaded", tc.engine)
			}
			if got := capabilitiesFor(m, tc.platform, tc.adopted, filepath.Join(base, tc.engine)); got != tc.want {
				t.Fatalf("capabilitiesFor(%q, %q, adopted=%t) = %+v, want %+v", tc.engine, tc.platform, tc.adopted, got, tc.want)
			}
		})
	}
}

// TestSetPortCapabilityMatchesEngineSetPortRules pins the derivation of
// set_port, which has to agree with what engine:set-port actually accepts: the
// port must appear in the startup inputs, and NVPAIR must be able to move an
// adopted engine. A port in a readiness probe or a stop command moves nothing.
func TestSetPortCapabilityMatchesEngineSetPortRules(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rt      Runtime
		adopted bool
		setPort bool
	}{
		{"process args", Runtime{Args: []string{"serve", "--port={port}"}}, false, true},
		{"process env", Runtime{Env: map[string]string{"OLLAMA_HOST": "{host}:{port}"}}, false, true},
		{"command start", Runtime{Mode: "command", Start: [][]string{{"cli", "up"}, {"cli", "serve", "{port}"}}}, false, true},
		{"probe only", Runtime{Ready: &Probe{TCP: "127.0.0.1:{port}"}}, false, false},
		{"stop only", Runtime{Stop: &StopSpec{Cmd: []string{"stop", "{port}"}}}, false, false},
		{"external process even with a stop spec", Runtime{Args: []string{"--port={port}"}, Stop: &StopSpec{Signal: "term"}}, true, false},
		{"external command without a stop command", Runtime{Mode: "command", Start: [][]string{{"start", "{port}"}}}, true, false},
		{"external command with an empty stop command", Runtime{Mode: "command", Start: [][]string{{"start", "{port}"}}, Stop: &StopSpec{}}, true, false},
		{"external command with an official stop command", Runtime{Mode: "command", Start: [][]string{{"start", "{port}"}}, Stop: &StopSpec{Cmd: []string{"stop"}}}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testEngineManifest(fakeEngineBin)
			m.Actions = nil // isolate set_port from the action flags
			for key, p := range m.Platforms {
				p.Runtime = tc.rt
				p.Uninstall = nil // isolate set_port from the uninstall flag
				m.Platforms[key] = p
			}
			got := capabilitiesFor(m, runtime.GOOS+"/"+runtime.GOARCH, tc.adopted, t.TempDir())
			if got.SetPort != tc.setPort {
				t.Errorf("set_port = %t, want %t (all flags %+v)", got.SetPort, tc.setPort, got)
			}
		})
	}
}

// TestCapabilitiesUseEffectiveManifest proves the derivation reads the effective
// manifest — the bundled one with the per-user override merged on — not the
// bundled file alone. This override pins Ollama's port into a fixed env value,
// so a port change can no longer reach the listener.
func TestCapabilitiesUseEffectiveManifest(t *testing.T) {
	dir := t.TempDir()
	override := []byte(`{"engine":"ollama","runtime":{"env":{"OLLAMA_HOST":"127.0.0.1:11434"}}}`)
	if err := os.WriteFile(filepath.Join(dir, "ollama.json"), override, 0o600); err != nil {
		t.Fatal(err)
	}
	ex := newBundledExecutor(t, dir)
	st, err := ex.state("ollama")
	if err != nil {
		t.Fatal(err)
	}
	assertCapabilities(t, ex.snapshot("ollama", st), Capabilities{
		ListModels: true, PullModel: true, DeleteModel: true,
		Install: true, Uninstall: true,
	})
}

// TestCapabilitiesOnEveryStatusPath proves each path that answers with an
// EngineStatus carries the capabilities: engine:status, engine:get-installed,
// and the engine:state-changed notification.
func TestCapabilitiesOnEveryStatusPath(t *testing.T) {
	m := testEngineManifest(fakeEngineBin)
	for key, p := range m.Platforms {
		p.Detect = nil        // nothing installed
		p.Runtime.Ready = nil // no probe, so no listener adoption can occur
		m.Platforms[key] = p
	}
	ex := newTestExecutor(t, m)
	want := Capabilities{ListModels: true, SetPort: true, Install: true}

	status, err := ex.Status(m.Engine)
	if err != nil {
		t.Fatal(err)
	}
	assertCapabilities(t, status, want)
	statuses := ex.GetInstalled()
	if len(statuses) != 1 {
		t.Fatalf("get-installed returned %d statuses, want 1", len(statuses))
	}
	assertCapabilities(t, statuses[0], want)

	var emitted EngineStatus
	ex.emit = func(method string, params any) {
		if method != "engine:state-changed" {
			return
		}
		var ok bool
		if emitted, ok = params.(EngineStatus); !ok {
			t.Fatalf("state notification payload type %T, want EngineStatus", params)
		}
	}
	ex.emitState(m.Engine)
	assertCapabilities(t, emitted, want)
	if !reflect.DeepEqual(emitted, status) {
		t.Errorf("state notification %+v differs from status %+v", emitted, status)
	}
}

// TestCapabilitiesOnSetPortResult covers the last path that builds a status
// itself: engine:set-port responds with the engine's resulting status.
func TestCapabilitiesOnSetPortResult(t *testing.T) {
	ex := newBundledExecutor(t, t.TempDir())
	st, err := ex.SetPort(context.Background(), "ollama", 12345)
	if err != nil {
		t.Fatalf("SetPort: %v", err)
	}
	assertCapabilities(t, st, Capabilities{
		ListModels: true, PullModel: true, DeleteModel: true,
		SetPort: true, Install: true, Uninstall: true,
	})
}

// TestUnsupportedPlatformReportsNoCapabilities covers an engine known to the
// registry with no block for this host: get-installed and status still list it,
// and report every flag false rather than a guess from another platform.
func TestUnsupportedPlatformReportsNoCapabilities(t *testing.T) {
	m := testEngineManifest(fakeEngineBin)
	m.Platforms = map[string]Platform{
		"unsupported/platform": m.Platforms[runtime.GOOS+"/"+runtime.GOARCH],
	}
	ex := newTestExecutor(t, m)

	status, err := ex.Status(m.Engine)
	if err != nil {
		t.Fatalf("Status for an unsupported platform should not error: %v", err)
	}
	if status.Engine != m.Engine || status.DisplayName != m.DisplayName {
		t.Errorf("unsupported-platform status = %+v, want the engine listed as unavailable", status)
	}
	assertCapabilities(t, status, Capabilities{})
	statuses := ex.GetInstalled()
	if len(statuses) != 1 {
		t.Fatalf("get-installed returned %d statuses, want 1", len(statuses))
	}
	assertCapabilities(t, statuses[0], Capabilities{})
}
