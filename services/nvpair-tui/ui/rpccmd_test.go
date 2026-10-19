// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"nvpair-tui/rpc"
)

func TestCallWithExplicitTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		v, c := engineWire(t)
		result := make(chan error, 1)
		cmd := callWithTimeout(v.client, "held", nil, 2*time.Second, func(_ *rpc.Message, err error) tea.Msg { result <- err; return nil })
		go cmd()
		if _, err := c.Read(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		select {
		case err := <-result:
			t.Fatalf("early completion: %v", err)
		default:
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timeout=%v", err)
		}
	})
}

// Hold real RPC replies while broker heartbeats keep the transport healthy.
// These exercise the operation's call sites, not just the timeout constants.
func TestEngineOperationLifetimes(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		remote    bool
		timeout   time.Duration
	}{
		{"local start", "s", false, 11 * time.Minute},
		{"local stop", "x", false, 11 * time.Minute},
		{"local restart", "r", false, 11 * time.Minute},
		{"local uninstall", "u", false, 35 * time.Second},
		{"local install", "i", false, 31 * time.Minute},
		{"local pull", "p", false, 31 * time.Minute},
		{"peer start", "s", true, 11 * time.Minute},
		{"peer stop", "x", true, 11 * time.Minute},
		{"peer install", "i", true, 31 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				v, c := engineWire(t)
				if tc.remote {
					switchEnginePeer(t, v, c)
				} else {
					v.Update(engineRequest(t, v.loadCmd(), c, "engine:get-installed", `null`, peerEngines))
				}
				cmd := v.Update(engineKey(tc.key))
				if tc.key == "p" {
					v.input.SetValue("fixture/model")
					cmd = v.submitPull()
				}
				if cmd == nil {
					t.Fatal("no operation command")
				}
				done := make(chan tea.Msg, 1)
				go func() { done <- cmd() }()
				if _, err := c.Read(); err != nil {
					t.Fatal(err)
				}
				remaining := tc.timeout - time.Second
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
					t.Fatalf("operation completed before its deadline: %#v", m)
				default:
				}
				time.Sleep(time.Second)
				synctest.Wait()
				select {
				case result := <-done:
					msg, ok := result.(engineOpMsg)
					if !ok || !errors.Is(msg.err, context.DeadlineExceeded) {
						t.Fatalf("expected deadline at %s: %#v", tc.timeout, result)
					}
				default:
					t.Fatalf("operation still pending past %s deadline", tc.timeout)
				}
			})
		})
	}
}

func TestEngineLocalLifecyclePendingPast35Seconds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		v, c := engineWire(t)
		v.Update(engineRequest(t, v.loadCmd(), c, "engine:get-installed", `null`, peerEngines))
		cmd := v.Update(engineKey("s"))
		done := make(chan tea.Msg, 1)
		go func() { done <- cmd() }()
		q, err := c.Read()
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(40 * time.Second)
		synctest.Wait()
		select {
		case m := <-done:
			t.Fatalf("lifecycle completed before reply: %#v", m)
		default:
		}
		if err := c.Respond(q.ID, nil); err != nil {
			t.Fatal(err)
		}
		v.Update(<-done)
		if v.status != "start custom ok" {
			t.Fatalf("status=%s", v.status)
		}
	})
}
