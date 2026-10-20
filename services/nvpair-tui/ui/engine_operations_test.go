// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"nvpair-tui/rpc"
)

const editableEngine = `{"engines":[{"engine":"custom","display_name":"Custom engine","installed":true,"running":true,"healthy":true,"port":1234,"capabilities":{"set_port":true,"install":true,"uninstall":true}}]}`

func localEditableEngine(t *testing.T) (*enginesView, *rpc.Codec) {
	t.Helper()
	v, c := engineWire(t)
	v.Update(engineRequest(t, v.loadCmd(), c, "engine:get-installed", `null`, editableEngine))
	return v, c
}

func enterKey() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyEnter} }

// Removing validation or sending before confirmation must fail these tests.
func TestEnginePortValidationAndConfirmation(t *testing.T) {
	for _, value := range []string{"0", "70000", "abc", "1.5", ""} {
		t.Run(value, func(t *testing.T) {
			v, _ := localEditableEngine(t)
			v.Update(engineKey("e"))
			if !v.CapturingInput() {
				t.Fatal("port editor did not capture input")
			}
			v.input.SetValue(value)
			if cmd := v.Update(enterKey()); cmd != nil {
				t.Fatal("invalid port dispatched")
			}
			if v.confirm.open() || !strings.Contains(v.View(), "1–65535") {
				t.Fatalf("missing validation: %s", v.View())
			}
		})
	}
	for _, value := range []string{"1", "65535", "2345"} {
		t.Run(value, func(t *testing.T) {
			v, c := localEditableEngine(t)
			v.Update(engineKey("e"))
			v.input.SetValue(value)
			if cmd := v.Update(enterKey()); cmd != nil {
				t.Fatal("port sent before y")
			}
			warning := "custom will restart on port " + value + " if it is running."
			if !v.CapturingInput() || !strings.Contains(v.View(), warning) {
				t.Fatalf("confirmation missing: %s", v.View())
			}
			// Async snapshots and cursor/target changes cannot retarget the command.
			v.Update(enginesLoadedMsg{generation: v.loadGeneration, engines: nil})
			if !strings.Contains(v.View(), warning) {
				t.Fatal("confirmation hidden after row removal")
			}
			reply := engineRequest(t, v.Update(engineKey("y")), c, "engine:set-port", `{"engine":"custom","port":`+value+`}`, `{"engine":"custom","port":`+value+`,"running":true}`)
			v.Update(reply)
			if !strings.Contains(v.View(), value) || v.byName["custom"].Port == 1234 {
				t.Fatalf("returned status not merged: %s", v.View())
			}
			if cmd := v.Update(engineKey("y")); cmd != nil {
				t.Fatal("duplicate set-port")
			}
		})
	}
}

func TestEnginePortAndUpdateCapabilityGates(t *testing.T) {
	for _, tc := range []struct {
		wire string
		peer bool
	}{
		{`{"engines":[{"engine":"omlx","capabilities":{"install":true,"uninstall":true,"set_port":false}}]}`, false},
		{`{"engines":[{"engine":"old"}]}`, false},
		{`{"engines":[{"engine":"custom","capabilities":{"install":true}}]}`, false},
		{editableEngine, true},
	} {
		v, c := engineWire(t)
		v.Update(engineRequest(t, v.loadCmd(), c, "engine:get-installed", `null`, tc.wire))
		if tc.peer {
			v.target = "peer-uuid"
		}
		e := v.byName[v.selectedEngine()]
		for _, k := range []string{"e", "U"} {
			allowed := !tc.peer && ((k == "e" && e.Capabilities.SetPort) || (k == "U" && e.Capabilities.Install && e.Capabilities.Uninstall))
			if allowed {
				continue
			}
			if cmd := v.Update(engineKey(k)); cmd != nil || v.CapturingInput() {
				t.Fatalf("unsupported %s prompted/dispatched", k)
			}
			for _, b := range v.Help() {
				for _, helpKey := range b.Keys() {
					if helpKey == k {
						t.Fatalf("unsupported help: %s", k)
					}
				}
			}
		}
	}
	v, _ := localEditableEngine(t)
	for _, k := range []string{"e", "U"} {
		found := false
		for _, b := range v.Help() {
			for _, helpKey := range b.Keys() {
				if helpKey == k {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("supported help absent: %s", k)
		}
	}
}

func engineErrorReply(t *testing.T, cmd tea.Cmd, c *rpc.Codec, method, params, detail string) tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatalf("%s returned no command", method)
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	q, err := c.Read()
	if err != nil {
		t.Fatal(err)
	}
	if q.Method != method || string(q.Params) != params {
		t.Fatalf("request=%s %s", q.Method, q.Params)
	}
	if err := c.RespondError(q.ID, -32000, detail); err != nil {
		t.Fatal(err)
	}
	return <-done
}

func TestEnginePortErrorAndFrozenInput(t *testing.T) {
	v, c := localEditableEngine(t)
	v.Update(engineKey("e"))
	v.merge(engineStatus{Engine: "other", Capabilities: engineCapabilities{SetPort: true}})
	v.table.SetCursor(1)
	v.input.SetValue("2345")
	v.Update(enterKey())
	reply := engineErrorReply(t, v.Update(engineKey("y")), c, "engine:set-port", `{"engine":"custom","port":2345}`, "port reserved")
	v.Update(reply)
	if !strings.Contains(v.View(), "port reserved") || v.byName["custom"].Port != 1234 {
		t.Fatalf("failed port: %s", v.View())
	}
}

func TestEngineUpdatePhasesAndFrozenTarget(t *testing.T) {
	for _, phase := range []string{"uninstall", "install", "success"} {
		t.Run(phase, func(t *testing.T) {
			v, c := localEditableEngine(t)
			if cmd := v.Update(engineKey("U")); cmd != nil {
				t.Fatal("update dispatched before y")
			}
			warning := "Update reinstalls custom: it is uninstalled, then installed and started. Inference stops meanwhile, and files the engine's uninstaller removes are deleted."
			if !v.CapturingInput() || !strings.Contains(v.View(), warning) {
				t.Fatalf("update warning: %s", v.View())
			}
			v.merge(engineStatus{Engine: "other"})
			v.table.SetCursor(1)
			cmd := v.Update(engineKey("y"))
			if cmd == nil {
				t.Fatal("no update command")
			}
			done := make(chan tea.Msg, 1)
			go func() { done <- cmd() }()
			q, err := c.Read()
			if err != nil {
				t.Fatal(err)
			}
			if q.Method != "engine:uninstall" || string(q.Params) != `{"engine":"custom"}` {
				t.Fatalf("first phase=%s %s", q.Method, q.Params)
			}
			if phase == "uninstall" {
				if err := c.RespondError(q.ID, -32000, "uninstall refused"); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := c.Respond(q.ID, json.RawMessage(`{"engine":"custom","installed":false}`)); err != nil {
					t.Fatal(err)
				}
			}
			// Uninstall failure must terminate the update; success must issue
			// install only after the uninstall reply.
			var result tea.Msg
			if phase == "uninstall" {
				result = <-done
			} else {
				q, err = c.Read()
				if err != nil {
					t.Fatal(err)
				}
				if q.Method != "engine:install" || string(q.Params) != `{"engine":"custom","start":true}` {
					t.Fatalf("second phase=%s %s", q.Method, q.Params)
				}
				if phase == "install" {
					err = c.RespondError(q.ID, -32000, "installer failed")
				} else {
					err = c.Respond(q.ID, json.RawMessage(`{"engine":"custom","installed":true,"running":true}`))
				}
				if err != nil {
					t.Fatal(err)
				}
				result = <-done
			}
			if next := v.Update(result); next != nil {
				t.Fatal("unexpected RPC after terminal update result")
			}
			want := "update custom ok"
			if phase != "success" {
				want = phase + " failed"
			}
			if !strings.Contains(v.View(), want) {
				t.Fatalf("phase outcome absent: %s", v.View())
			}
			if cmd := v.Update(engineKey("y")); cmd != nil {
				t.Fatal("update repeated")
			}
		})
	}
}

func TestEngineNewPromptsDefaultCancel(t *testing.T) {
	for _, k := range []string{"e", "U", "u"} {
		for _, cancel := range []tea.KeyMsg{engineKey("n"), {Type: tea.KeyEsc}, enterKey()} {
			v, _ := localEditableEngine(t)
			if cmd := v.Update(engineKey(k)); k != "e" && cmd != nil {
				t.Fatalf("%s sent before confirmation", k)
			}
			if k == "e" {
				v.input.SetValue("2345")
				v.Update(enterKey())
			}
			if !v.CapturingInput() {
				t.Fatalf("%s did not confirm", k)
			}
			if cmd := v.Update(cancel); cmd != nil || v.CapturingInput() {
				t.Fatalf("%s did not cancel", k)
			}
			if cmd := v.Update(engineKey("y")); cmd != nil {
				t.Fatal("cancelled action sent")
			}
		}
	}
}

func TestEngineUninstallFrozen(t *testing.T) {
	v, c := localEditableEngine(t)
	if cmd := v.Update(engineKey("u")); cmd != nil {
		t.Fatal("uninstall sent before y")
	}
	if !strings.Contains(v.View(), "custom") || !v.CapturingInput() {
		t.Fatal("uninstall prompt absent")
	}
	v.Update(enginesLoadedMsg{generation: v.loadGeneration, engines: []engineStatus{{Engine: "other"}}})
	reply := engineRequest(t, v.Update(engineKey("y")), c, "engine:uninstall", `{"engine":"custom"}`, `{"engine":"custom","installed":false}`)
	v.Update(reply)
	if cmd := v.Update(engineKey("y")); cmd != nil {
		t.Fatal("uninstall repeated")
	}
}

func TestEnginePortAndUninstallLifetimes(t *testing.T) {
	for _, k := range []string{"e", "u"} {
		t.Run(k, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				v, c := localEditableEngine(t)
				v.Update(engineKey(k))
				if k == "e" {
					v.input.SetValue("2345")
					v.Update(enterKey())
				}
				cmd := v.Update(engineKey("y"))
				if cmd == nil {
					t.Fatal("no confirmed command")
				}
				done := make(chan tea.Msg, 1)
				go func() { done <- cmd() }()
				if _, err := c.Read(); err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 21; i++ {
					time.Sleep(30 * time.Second)
					if err := c.Notify("heartbeat", nil); err != nil {
						t.Fatal(err)
					}
				}
				time.Sleep(29 * time.Second)
				synctest.Wait()
				select {
				case m := <-done:
					t.Fatalf("early timeout: %#v", m)
				default:
				}
				time.Sleep(time.Second)
				synctest.Wait()
				v.Update(<-done)
				if !strings.Contains(v.status, "context deadline exceeded") {
					t.Fatalf("missing lifecycle timeout: %s", v.status)
				}
			})
		})
	}
}
