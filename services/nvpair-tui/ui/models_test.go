// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"nvpair-tui/rpc"
)

const modelInventory = `{"models":["alpha","beta"],"modelsByEngine":{"lmstudio":["alpha","beta"]},"loadedByEngine":{"lmstudio":["beta"]}}`

func modelEngine(v *enginesView, engine string) {
	v.merge(engineStatus{Engine: engine, Capabilities: engineCapabilities{ListModels: true, PullModel: true, DeleteModel: true, DeleteRestartsEngine: engine == "lmstudio"}})
}

func openLocalModels(t *testing.T, v *enginesView, c *rpc.Codec, inventory string) {
	t.Helper()
	v.Update(engineRequest(t, v.Update(engineKey("m")), c, "engine:models", `null`, inventory))
}

func modelPush(v *enginesView, inventory string) {
	v.Update(NotificationMsg{Msg: &rpc.Message{Method: "engine:models-changed", Params: json.RawMessage(`{"engine":"lmstudio","models":` + inventory + `}`)}})
}

func TestModelInventoryPane(t *testing.T) {
	v, c := engineWire(t)
	modelEngine(v, "lmstudio")
	openLocalModels(t, v, c, modelInventory)
	for _, want := range []string{"alpha", "beta", "loaded"} {
		if !strings.Contains(v.View(), want) {
			t.Fatalf("missing %s: %s", want, v.View())
		}
	}
	// A full normalized notification, not the flat union, replaces the pane.
	modelPush(v, `{"modelsByEngine":{"lmstudio":[]},"loadedByEngine":{}}`)
	if !strings.Contains(v.View(), "No models") {
		t.Fatalf("empty inventory: %s", v.View())
	}
	modelPush(v, `{"models":["other-engine"],"modelsByEngine":{}}`)
	if !strings.Contains(v.View(), "Model list unavailable (engine stopped or unsupported)") {
		t.Fatalf("absent engine: %s", v.View())
	}
	if cmd := v.Update(engineKey("d")); cmd != nil || v.CapturingInput() {
		t.Fatal("delete without a model")
	}
	v.Update(engineKey("m"))
	if strings.Contains(v.View(), "Model list unavailable") {
		t.Fatal("m did not close models pane")
	}
}

func TestModelCapabilitiesGateLocalAndPeer(t *testing.T) {
	for _, remote := range []bool{false, true} {
		for _, wire := range []string{`{"engine":"omlx","capabilities":{"list_models":true,"pull_model":false,"delete_model":false}}`, `{"engine":"older-peer"}`} {
			v, c := engineWire(t)
			if remote {
				v.target = "peer-uuid"
			}
			var e engineStatus
			if err := json.Unmarshal([]byte(wire), &e); err != nil {
				t.Fatal(err)
			}
			v.merge(e)
			if remote {
				v.Update(NotificationMsg{Msg: &rpc.Message{Method: "discovery:nodes-changed", Params: json.RawMessage(`[{"hostUuid":"peer-uuid","modelsByEngine":{"omlx":["read-only"]}}]`)}})
				v.Update(engineKey("m"))
			} else {
				openLocalModels(t, v, c, `{"modelsByEngine":{"omlx":["read-only"]}}`)
			}
			for _, k := range []string{"p", "d"} {
				if cmd := v.Update(engineKey(k)); cmd != nil || v.CapturingInput() {
					t.Fatalf("unsupported %s acted remote=%v", k, remote)
				}
				for _, b := range v.Help() {
					for _, hk := range b.Keys() {
						if hk == k {
							t.Fatalf("unsupported help %s", k)
						}
					}
				}
			}
		}
	}
}

func TestModelDeleteConfirmFrozenAndExactParams(t *testing.T) {
	for _, engine := range []string{"lmstudio", "ollama"} {
		t.Run(engine, func(t *testing.T) {
			v, c := engineWire(t)
			modelEngine(v, engine)
			inv := strings.ReplaceAll(modelInventory, "lmstudio", engine)
			openLocalModels(t, v, c, inv)
			if cmd := v.Update(engineKey("d")); cmd != nil || !v.CapturingInput() {
				t.Fatal("delete must prompt without RPC")
			}
			for _, want := range []string{"This node", engine, "alpha"} {
				if !strings.Contains(v.View(), want) {
					t.Fatalf("prompt missing %s: %s", want, v.View())
				}
			}
			if engine == "lmstudio" && !strings.Contains(v.View(), "This restarts lmstudio; running inference will be interrupted.") {
				t.Fatalf("restart warning: %s", v.View())
			}
			// A refresh replaces the selected row while the prompt captures all keys.
			modelPush(v, `{"modelsByEngine":{"`+engine+`":["replacement"]}}`)
			if cmd := v.Update(tea.KeyMsg{Type: tea.KeyTab}); cmd != nil || v.target != "" {
				t.Fatal("prompt allowed target switch")
			}
			param := `{"model":"alpha"}`
			if engine == "ollama" {
				param = `{"name":"alpha"}`
			}
			reply := engineRequest(t, v.Update(engineKey("y")), c, "engine:action", `{"engine":"`+engine+`","action":"delete_model","params":`+param+`}`, `null`)
			refresh := v.Update(reply)
			if v.CapturingInput() {
				t.Fatal("confirmation did not close")
			}
			v.Update(engineRequest(t, refresh, c, "engine:models", `null`, `{"modelsByEngine":{"`+engine+`":[]}}`))
			if !strings.Contains(v.View(), "No models") {
				t.Fatalf("delete failed to refresh: %s", v.View())
			}
		})
	}
}

func TestModelDeleteDefaultCancel(t *testing.T) {
	for _, k := range []tea.KeyMsg{{Type: tea.KeyEnter}, {Type: tea.KeyEsc}, engineKey("n")} {
		v, c := engineWire(t)
		modelEngine(v, "lmstudio")
		openLocalModels(t, v, c, modelInventory)
		v.Update(engineKey("d"))
		if cmd := v.Update(k); cmd != nil || v.CapturingInput() {
			t.Fatal("cancel sent RPC or left prompt open")
		}
		if cmd := v.Update(engineKey("y")); cmd != nil {
			t.Fatal("cancelled delete resurrected")
		}
	}
}

func TestModelPeerInventoryDeleteAndDiscoveryRefresh(t *testing.T) {
	v, c := engineWire(t)
	switchEnginePeer(t, v, c)
	modelEngine(v, "custom")
	v.Update(NotificationMsg{Msg: &rpc.Message{Method: "discovery:nodes-changed", Params: json.RawMessage(`[{"hostUuid":"peer-uuid","name":"GPU-box","trusted":true,"modelsByEngine":{"custom":["peer-model"]},"loadedByEngine":{"custom":["peer-model"]}}]`)}})
	if cmd := v.Update(engineKey("m")); cmd != nil {
		t.Fatal("peer inventory must use cached discovery")
	}
	if !strings.Contains(v.View(), "peer-model") || !strings.Contains(v.View(), "loaded") {
		t.Fatalf("peer inventory: %s", v.View())
	}
	modelPush(v, modelInventory)
	if strings.Contains(v.View(), "alpha") {
		t.Fatal("local inventory contaminated peer")
	}
	v.Update(engineKey("d"))
	if !strings.Contains(v.View(), "GPU-box") {
		t.Fatalf("peer delete prompt: %s", v.View())
	}
	// Freeze node, engine and model, even if the live discovery row disappears.
	v.Update(discoverySnapshotMsg{nodes: nil})
	reply := engineRequest(t, v.Update(engineKey("y")), c, "engine:remote-delete-model", `{"node":"peer-uuid","engine":"custom","model":"peer-model"}`, `{"ok":true}`)
	v.Update(engineRequest(t, v.Update(reply), c, "discovery:get-nodes", `null`, `{"nodes":[{"hostUuid":"peer-uuid","modelsByEngine":{"custom":[]}}]}`))
	if !strings.Contains(v.View(), "No models") {
		t.Fatalf("peer delete did not refresh discovery inventory: %s", v.View())
	}
	v.Update(engineKey("m"))
	v.Update(engineKey("m"))
	if !strings.Contains(v.View(), "No models") {
		t.Fatalf("reopening resurrected pre-delete discovery: %s", v.View())
	}
}

func TestModelDeleteRestartFailureIsPartialSuccess(t *testing.T) {
	for _, remote := range []bool{false, true} {
		v, c := engineWire(t)
		modelEngine(v, "lmstudio")
		if remote {
			v.target = "peer-uuid"
			v.Update(NotificationMsg{Msg: &rpc.Message{Method: "discovery:nodes-changed", Params: json.RawMessage(`[{"hostUuid":"peer-uuid","modelsByEngine":{"lmstudio":["alpha"]}}]`)}})
			v.Update(engineKey("m"))
		} else {
			openLocalModels(t, v, c, modelInventory)
		}
		v.Update(engineKey("d"))
		cmd := v.Update(engineKey("y"))
		if cmd == nil {
			t.Fatal("missing confirmed delete")
		}
		done := make(chan tea.Msg, 1)
		go func() { done <- cmd() }()
		q, err := c.Read()
		if err != nil {
			t.Fatal(err)
		}
		detail := `action "delete_model": engine "lmstudio" failed to restart: readiness failed`
		if remote {
			detail = "remote /v1/models/delete: HTTP 500: " + detail
		}
		if err := c.RespondError(q.ID, -32000, detail); err != nil {
			t.Fatal(err)
		}
		refresh := v.Update(<-done)
		if !strings.Contains(v.View(), "deleted, restart failed") || strings.Contains(v.status, "delete alpha lmstudio failed:") {
			t.Fatalf("misleading delete result: %s", v.View())
		}
		if refresh == nil {
			t.Fatal("partial success must refresh inventory")
		}
	}
}

func TestModelPullWireAndRefresh(t *testing.T) {
	for _, remote := range []bool{false, true} {
		v, c := engineWire(t)
		modelEngine(v, "lmstudio")
		if remote {
			v.target = "peer-uuid"
		}
		if cmd := v.Update(engineKey("p")); cmd == nil || !v.CapturingInput() {
			t.Fatal("supported pull did not prompt")
		}
		v.input.SetValue(" downloaded ")
		method, params := "engine:action", `{"engine":"lmstudio","action":"pull_model","params":{"name":"downloaded","model":"downloaded"}}`
		if remote {
			method, params = "engine:remote-pull-model", `{"node":"peer-uuid","engine":"lmstudio","model":"downloaded"}`
		}
		reply := engineRequest(t, v.Update(tea.KeyMsg{Type: tea.KeyEnter}), c, method, params, `null`)
		refresh := v.Update(reply)
		if remote {
			v.Update(engineRequest(t, refresh, c, "discovery:get-nodes", `null`, `{"nodes":[]}`))
		} else {
			v.Update(engineRequest(t, refresh, c, "engine:models", `null`, modelInventory))
		}
		if !strings.Contains(v.status, "ok") {
			t.Fatalf("pull result: %s", v.status)
		}
	}
}

func TestModelInventoryStaleGetterCannotOverwritePush(t *testing.T) {
	v, c := engineWire(t)
	modelEngine(v, "lmstudio")
	pending := v.Update(engineKey("m"))
	modelPush(v, modelInventory)
	stale := engineRequest(t, pending, c, "engine:models", `null`, `{"modelsByEngine":{"lmstudio":[]}}`)
	v.Update(stale)
	if !strings.Contains(v.View(), "alpha") {
		t.Fatalf("stale getter overwrote notification: %s", v.View())
	}
}

func TestModelOperationLifetimes(t *testing.T) {
	for _, remote := range []bool{false, true} {
		for _, op := range []string{"p", "d"} {
			t.Run(strings.Join([]string{map[bool]string{false: "local", true: "peer"}[remote], op}, "/"), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					v, c := engineWire(t)
					modelEngine(v, "lmstudio")
					if remote {
						v.target = "peer-uuid"
						v.Update(NotificationMsg{Msg: &rpc.Message{Method: "discovery:nodes-changed", Params: json.RawMessage(`[{"hostUuid":"peer-uuid","modelsByEngine":{"lmstudio":["alpha"]}}]`)}})
						v.Update(engineKey("m"))
					} else {
						openLocalModels(t, v, c, modelInventory)
					}
					v.Update(engineKey(op))
					timeout := 11 * time.Minute
					var cmd tea.Cmd
					if op == "p" {
						v.input.SetValue("alpha")
						cmd = v.Update(tea.KeyMsg{Type: tea.KeyEnter})
						timeout = 31 * time.Minute
					} else {
						cmd = v.Update(engineKey("y"))
					}
					if cmd == nil {
						t.Fatal("no operation command")
					}
					done := make(chan tea.Msg, 1)
					go func() { done <- cmd() }()
					if _, err := c.Read(); err != nil {
						t.Fatal(err)
					}
					remaining := timeout - time.Second
					for remaining > 0 {
						step := min(30*time.Second, remaining)
						time.Sleep(step)
						remaining -= step
						if err := c.Notify("heartbeat", nil); err != nil {
							t.Fatal(err)
						}
					}
					synctest.Wait()
					select {
					case m := <-done:
						t.Fatalf("early completion: %#v", m)
					default:
					}
					time.Sleep(time.Second)
					synctest.Wait()
					select {
					case m := <-done:
						// Both operation message types expose the timeout through the rendered outcome.
						v.Update(m)
						if !strings.Contains(v.status, context.DeadlineExceeded.Error()) {
							t.Fatalf("not deadline: %s", v.status)
						}
					default:
						t.Fatalf("pending past %s", timeout)
					}
				})
			})
		}
	}
}
