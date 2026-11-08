// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"strings"
	"testing"

	settings "nvpair-shared/enginesettings"
	"nvpair-tui/rpc"
)

func TestEngineEffectivePortsLoadAndOrdering(t *testing.T) {
	v, c := engineWire(t)
	v.merge(engineStatus{Engine: "ollama"})
	msg := engineRequest(t, v.loadEnginePortsCmd(), c, "engine:get-settings", `{"engine":"ollama"}`, `{"engine":"ollama","epoch":"one","sequence":1,"settings":{"serverPort":11435,"proxyPort":11434},"effectiveProxyPort":11500}`)
	v.Update(msg)
	if !strings.Contains(v.View(), "11500 because 11434 was busy") {
		t.Fatal("initial settings read did not show the runtime port")
	}
	v.receiveEnginePorts(settings.Snapshot{Engine: "ollama", Epoch: "one", Sequence: 2})
	v.Update(msg)
	if strings.Contains(v.View(), "Running on port") {
		t.Fatal("delayed settings read overwrote a newer push")
	}
	v.Update(epochChangedMsg{})
	v.Update(msg)
	if strings.Contains(v.View(), "Running on port") {
		t.Fatal("old read survived epoch recovery")
	}
	v.target = "peer"
	if v.loadEnginePortsCmd() != nil {
		t.Fatal("peer status does not supply configured/effective port pairs")
	}
}

func TestEnginePortEditorKeepsConfiguredPort(t *testing.T) {
	v := newEnginesView(nil)
	v.SetSize(120, 30)
	v.merge(engineStatus{Engine: "ollama", Port: 11501, Capabilities: engineCapabilities{SetPort: true}})
	v.receiveEnginePorts(settings.Snapshot{Engine: "ollama", Settings: settings.Config{ServerPort: 11435}, EffectiveServerPort: 11501})
	v.Update(engineKey("e"))
	if v.input.Value() != "11435" {
		t.Fatalf("editor offered runtime fallback %s instead of configured port", v.input.Value())
	}
}

func TestEngineEffectivePortNotes(t *testing.T) {
	for _, engine := range []string{"ollama", "lmstudio", "omlx", "llamacpp"} {
		t.Run(engine, func(t *testing.T) {
			v := newEnginesView(nil)
			v.SetSize(120, 30)
			v.merge(engineStatus{Engine: engine, Running: true, Port: 11501})
			snapshot := settings.Snapshot{Engine: engine, Epoch: "one", Sequence: 1,
				Settings:            settings.Config{ServerPort: 11435, ProxyPort: 11434},
				EffectiveServerPort: 11501, EffectiveProxyPort: 11500}
			push := func() {
				t.Helper()
				raw, err := json.Marshal(snapshot)
				if err != nil {
					t.Fatal(err)
				}
				v.Update(NotificationMsg{Msg: &rpc.Message{Method: "engine:settings-changed", Params: raw}})
			}
			push()
			for _, note := range []string{
				"Server port: Running on port 11501 because 11435 was busy. PAIR will try 11435 again next start.",
				"Proxy port: Running on port 11500 because 11434 was busy. PAIR will try 11434 again next start.",
			} {
				if !strings.Contains(v.View(), note) {
					t.Fatalf("missing %q in %s", note, v.View())
				}
			}
			snapshot.Sequence++
			snapshot.EffectiveServerPort, snapshot.EffectiveProxyPort = 11435, 11434
			push()
			if strings.Contains(v.View(), "Running on port") {
				t.Fatal("matching ports showed a note")
			}
			snapshot.Sequence++
			snapshot.EffectiveServerPort, snapshot.EffectiveProxyPort = 0, 0
			push()
			if strings.Contains(v.View(), "Running on port") {
				t.Fatal("unknown ports showed a note")
			}
			snapshot.Sequence++
			snapshot.EffectiveProxyPort = 11500
			push()
			v.target = "peer"
			if strings.Contains(v.View(), "Running on port") {
				t.Fatal("local note leaked into peer view")
			}
			v.target = ""
			v.Update(epochChangedMsg{})
			if strings.Contains(v.View(), "Running on port") {
				t.Fatal("old epoch note remained visible")
			}
		})
	}
}
