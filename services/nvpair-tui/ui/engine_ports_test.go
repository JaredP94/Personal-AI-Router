// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	settings "nvpair-shared/enginesettings"
	"nvpair-tui/rpc"
)

func TestEnginePortsWaitForLocalIdentity(t *testing.T) {
	v, c := engineWire(t)
	v.merge(engineStatus{Engine: "ollama"})
	snapshot := settings.Snapshot{NodeID: "local", Engine: "ollama", Epoch: "one", Sequence: 1,
		Settings: settings.Config{ProxyPort: 11434}, EffectiveProxyPort: 11500}
	v.receiveEnginePorts(snapshot)
	if len(v.enginePorts) != 0 {
		t.Fatal("cached settings without a known local authority")
	}
	cmd := v.Update(clusterIdentityMsg{id: clusterIdentity{NodeUUID: "local", NodeID: "local-host"}})
	msg := engineRequest(t, cmd, c, "engine:get-settings", `{"engine":"ollama"}`, `{"nodeId":"local","engine":"ollama","epoch":"one","sequence":1,"settings":{"proxyPort":11434},"effectiveProxyPort":11500}`)
	v.Update(msg)
	if !strings.Contains(v.enginePortNotes(), "Running on port 11500") {
		t.Fatal("learning local identity did not recover the port snapshot")
	}
	snapshot.NodeID = "local-host"
	snapshot.Sequence = 99
	v.receiveEnginePorts(snapshot)
	if v.enginePorts["ollama"].Sequence != 1 {
		t.Fatal("accepted host name instead of stable local UUID")
	}
	v.Update(epochChangedMsg{})
	v.receiveEnginePorts(snapshot)
	if len(v.enginePorts) != 0 || v.identity.NodeUUID != "" {
		t.Fatal("retained the old authority across an epoch change")
	}
}

func TestEnginePortsIgnorePeerSettings(t *testing.T) {
	for _, editing := range []bool{false, true} {
		t.Run(fmt.Sprintf("editing=%t", editing), func(t *testing.T) {
			v := newEnginesView(nil)
			v.SetSize(120, 30)
			v.identity = clusterIdentity{NodeUUID: "local", NodeID: "local-host"}
			v.merge(engineStatus{Engine: "ollama", Running: true, Capabilities: engineCapabilities{SetPort: true}})
			push := func(node string, sequence uint64, configured, effective int) {
				t.Helper()
				raw, err := json.Marshal(settings.Snapshot{NodeID: node, Engine: "ollama", Epoch: "one", Sequence: sequence, Running: true,
					Settings: settings.Config{ServerPort: configured}, EffectiveServerPort: effective})
				if err != nil {
					t.Fatal(err)
				}
				v.Update(NotificationMsg{Msg: &rpc.Message{Method: "engine:settings-changed", Params: raw}})
			}
			push("local", 1, 11435, 11501)
			before := v.enginePortNotes()
			if editing {
				v.Update(engineKey("e"))
				v.input.SetValue("12000")
			}
			push("peer", 99, 22000, 22001)
			if v.enginePortNotes() != before {
				t.Fatal("peer settings overwrote the local port note")
			}
			if editing {
				if v.input.Value() != "12000" {
					t.Fatal("peer settings changed the active local draft")
				}
				v.Update(engineKey("esc"))
			}
			v.Update(engineKey("e"))
			if v.input.Value() != "11435" {
				t.Fatalf("local editor suggested peer port %s", v.input.Value())
			}
			push("local", 2, 11436, 11502)
			if v.enginePorts["ollama"].Settings.ServerPort != 11436 {
				t.Fatal("peer sequence suppressed a later local update")
			}
		})
	}
}

func TestEngineEffectivePortsLoadAndOrdering(t *testing.T) {
	v, c := engineWire(t)
	v.identity = clusterIdentity{NodeUUID: "local"}
	v.merge(engineStatus{Engine: "ollama"})
	msg := engineRequest(t, v.loadEnginePortsCmd(), c, "engine:get-settings", `{"engine":"ollama"}`, `{"nodeId":"local","engine":"ollama","epoch":"one","sequence":1,"settings":{"serverPort":11435,"proxyPort":11434},"effectiveProxyPort":11500}`)
	v.Update(msg)
	if !strings.Contains(v.View(), "11500 (saved setting: 11434)") {
		t.Fatal("initial settings read did not show the runtime port")
	}
	v.receiveEnginePorts(settings.Snapshot{NodeID: "local", Engine: "ollama", Epoch: "one", Sequence: 2})
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

func TestEnginePortEditorDoesNotSuggestUnknownConfiguredPort(t *testing.T) {
	v := newEnginesView(nil)
	v.SetSize(120, 30)
	v.merge(engineStatus{Engine: "ollama", Port: 11501, Capabilities: engineCapabilities{SetPort: true}})
	v.Update(engineKey("e"))
	if v.input.Value() != "" {
		t.Fatalf("editor suggested runtime port %s before configured settings arrived", v.input.Value())
	}
}

func TestEnginePortEditorKeepsConfiguredPort(t *testing.T) {
	v := newEnginesView(nil)
	v.SetSize(120, 30)
	v.merge(engineStatus{Engine: "ollama", Port: 11501, Capabilities: engineCapabilities{SetPort: true}})
	v.identity = clusterIdentity{NodeUUID: "local"}
	v.receiveEnginePorts(settings.Snapshot{NodeID: "local", Engine: "ollama", Settings: settings.Config{ServerPort: 11435}, EffectiveServerPort: 11501})
	v.Update(engineKey("e"))
	if v.input.Value() != "11435" {
		t.Fatalf("editor offered runtime fallback %s instead of configured port", v.input.Value())
	}
}

func TestEngineManagedLMStudioPortNote(t *testing.T) {
	v := newEnginesView(nil)
	v.SetSize(120, 30)
	v.identity = clusterIdentity{NodeUUID: "local"}
	v.merge(engineStatus{Engine: "lmstudio", Running: true})
	// A non-explicit legacy saved proxy port can differ under managed ownership,
	// even when it was free. The snapshot does not report a cause or retry policy.
	v.receiveEnginePorts(settings.Snapshot{NodeID: "local", Engine: "lmstudio", Running: true,
		Settings:            settings.Config{ServerPort: 1235, ProxyPort: 12420},
		EffectiveServerPort: 1235, EffectiveProxyPort: 1234})
	if !strings.Contains(v.View(), "Proxy port: Running on port 1234 (saved setting: 12420).") {
		t.Fatalf("missing neutral managed-port note: %s", v.View())
	}
	if strings.Contains(v.View(), "busy") || strings.Contains(v.View(), "next start") {
		t.Fatal("port mismatch invented a cause or retry promise")
	}
}

func TestEngineEffectivePortNotes(t *testing.T) {
	for _, engine := range []string{"ollama", "lmstudio", "omlx", "llamacpp"} {
		t.Run(engine, func(t *testing.T) {
			v := newEnginesView(nil)
			v.SetSize(120, 30)
			v.merge(engineStatus{Engine: engine, Running: true, Port: 11501})
			v.identity = clusterIdentity{NodeUUID: "local"}
			snapshot := settings.Snapshot{NodeID: "local", Engine: engine, Epoch: "one", Sequence: 1, Running: true,
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
				"Server port: Running on port 11501 (saved setting: 11435).",
				"Proxy port: Running on port 11500 (saved setting: 11434).",
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
			snapshot.Running, snapshot.EffectiveServerPort = false, 11501
			push()
			if strings.Contains(v.View(), "Running on port") {
				t.Fatal("stopped server's retained port showed a note")
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
