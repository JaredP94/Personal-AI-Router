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

func TestEngineUpdateWaitsForUninstallSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		v, c := localEditableEngine(t)
		v.Update(engineKey("U"))
		cmd := v.Update(engineKey("y"))
		if cmd == nil {
			t.Fatal("no confirmed update")
		}
		done := make(chan tea.Msg, 1)
		go func() { done <- cmd() }()
		q, err := c.Read()
		if err != nil {
			t.Fatal(err)
		}
		if q.Method != "engine:uninstall" {
			t.Fatalf("first request=%s", q.Method)
		}
		next := make(chan *rpc.Message, 1)
		go func() {
			q, err := c.Read()
			if err == nil {
				next <- q
			}
		}()
		time.Sleep(40 * time.Second)
		synctest.Wait()
		select {
		case q := <-next:
			t.Fatalf("install before uninstall success: %s", q.Method)
		default:
		}
		select {
		case m := <-done:
			t.Fatalf("update completed before uninstall reply: %#v", m)
		default:
		}
		if err := c.RespondError(q.ID, -32000, "uninstall refused"); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		select {
		case m := <-done:
			v.Update(m)
		default:
			t.Fatal("failed uninstall did not terminate update")
		}
		select {
		case q := <-next:
			t.Fatalf("install after failed uninstall: %s", q.Method)
		default:
		}
		if !strings.Contains(v.status, "uninstall failed") {
			t.Fatalf("wrong phase: %s", v.status)
		}
	})
}

// An update has one 31-minute lifetime, including uninstall. Uninstall is
// additionally capped at the lifecycle lifetime; install uses the remainder.
func TestEngineUpdateDeadlines(t *testing.T) {
	for _, phase := range []string{"uninstall", "install"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				v, c := localEditableEngine(t)
				v.Update(engineKey("U"))
				cmd := v.Update(engineKey("y"))
				if cmd == nil {
					t.Fatal("no update command")
				}
				done := make(chan tea.Msg, 1)
				go func() { done <- cmd() }()
				started := time.Now()
				q, err := c.Read()
				if err != nil {
					t.Fatal(err)
				}
				deadline := 11 * time.Minute
				if phase == "install" {
					// Time consumed by uninstall counts against the overall update budget.
					time.Sleep(30 * time.Second)
					if err := c.Respond(q.ID, json.RawMessage(`{"engine":"custom","installed":false}`)); err != nil {
						t.Fatal(err)
					}
					q, err = c.Read()
					if err != nil {
						t.Fatal(err)
					}
					if q.Method != "engine:install" {
						t.Fatalf("wrong next phase: %s", q.Method)
					}
					deadline = 31 * time.Minute
				}
				remaining := deadline - time.Since(started) - time.Second
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
					t.Fatalf("early update completion: %#v", m)
				default:
				}
				time.Sleep(time.Second)
				synctest.Wait()
				select {
				case m := <-done:
					v.Update(m)
					if !strings.Contains(v.status, phase+" failed: context deadline exceeded") {
						t.Fatalf("wrong timeout phase: %s", v.status)
					}
				default:
					t.Fatal("update did not terminate at deadline")
				}
			})
		})
	}
}

func TestEngineConfirmedLocalActionsDoNotRetargetPeer(t *testing.T) {
	for _, k := range []string{"e", "u", "U"} {
		t.Run(k, func(t *testing.T) {
			v, c := localEditableEngine(t)
			v.Update(engineKey(k))
			if k == "e" {
				v.input.SetValue("2345")
				v.Update(enterKey())
			}
			// A programmatic target replacement while the confirmation is open must
			// not rewrite the captured local request or populate the peer with its reply.
			v.target = "peer-uuid"
			v.order = nil
			v.byName = map[string]engineStatus{}
			v.status = "peer selected"
			cmd := v.Update(engineKey("y"))
			if cmd == nil {
				t.Fatal("frozen local confirmation lost")
			}
			var result tea.Msg
			switch k {
			case "e":
				result = engineRequest(t, cmd, c, "engine:set-port", `{"engine":"custom","port":2345}`, `{"engine":"custom","port":2345}`)
			case "u":
				result = engineRequest(t, cmd, c, "engine:uninstall", `{"engine":"custom"}`, `{"engine":"custom"}`)
			case "U":
				result = engineErrorReply(t, cmd, c, "engine:uninstall", `{"engine":"custom"}`, "refused")
			}
			if cmd := v.Update(result); cmd != nil || v.status != "peer selected" || len(v.order) != 0 {
				t.Fatalf("local result contaminated peer: %s %v", v.status, v.order)
			}
		})
	}
}
