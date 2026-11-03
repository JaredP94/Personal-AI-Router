// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEngineSettingReadRelay(t *testing.T) {
	for _, mode := range []string{"true", "false", "missing", "rpc-error", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			engine, replies := newTestRPCWorkerPipe(t)
			b := &Broker{}
			b.setEngineMgr(engine)
			method := "settings/get-engine-allow-lan-bind"
			if mode == "unsupported" {
				method = "settings/set-engine-allow-lan-bind"
			}
			if mode != "missing" && mode != "unsupported" {
				settings, requests := newTestRPCWorkerPipe(t)
				b.setSettings(settings)
				go func() {
					request, err := requests.Read()
					if err != nil {
						return
					}
					if request.Method != method {
						_ = requests.RespondError(request.ID, -32601, "wrong method")
						return
					}
					if mode == "rpc-error" {
						_ = requests.RespondError(request.ID, -32603, "read failed")
						return
					}
					_ = requests.Respond(request.ID, map[string]bool{"value": mode == "true"})
				}()
			}
			raw, _ := json.Marshal(map[string]string{"id": "read-1", "method": method})
			b.forwardEngineNotification("engine:settings-request", raw)
			done := make(chan *Message, 1)
			go func() { msg, _ := replies.Read(); done <- msg }()
			select {
			case msg := <-done:
				if msg == nil || msg.Method != "engine:settings-reply" {
					t.Fatalf("unexpected reply %+v", msg)
				}
				var reply struct {
					ID     string
					Result json.RawMessage
					Error  string
				}
				if err := json.Unmarshal(msg.Params, &reply); err != nil {
					t.Fatal(err)
				}
				if reply.ID != "read-1" {
					t.Fatalf("uncorrelated reply %+v", reply)
				}
				if mode == "true" || mode == "false" {
					var value struct{ Value bool }
					if json.Unmarshal(reply.Result, &value) != nil || value.Value != (mode == "true") || reply.Error != "" {
						t.Fatalf("bad result %+v", reply)
					}
				} else if reply.Error == "" {
					t.Fatal("missing failure")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("setting read relay did not finish")
			}
		})
	}
}
