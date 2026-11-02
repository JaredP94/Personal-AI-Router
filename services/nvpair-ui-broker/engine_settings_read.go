// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"time"
)

// handleEngineSettingsRead services the worker's narrow live permission read.
// Capture the worker before dispatch so a response never crosses a respawn.
func (b *Broker) handleEngineSettingsRead(raw json.RawMessage) {
	var request struct {
		ID     string `json:"id"`
		Method string `json:"method"`
	}
	if json.Unmarshal(raw, &request) != nil || request.ID == "" {
		return
	}
	em := b.getEngineMgr()
	if em == nil {
		return
	}
	settings := b.getSettings()
	go func() {
		var reply struct {
			ID     string          `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  string          `json:"error,omitempty"`
		}
		reply.ID = request.ID
		switch {
		case request.Method != "settings/get-engine-allow-lan-bind":
			reply.Error = "unsupported engine setting read"
		case settings == nil:
			reply.Error = "node-settings unavailable"
		default:
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			result, rpcErr, err := settings.Call(ctx, request.Method, nil)
			switch {
			case err != nil:
				reply.Error = "node-settings read failed"
			case rpcErr != nil:
				reply.Error = "node-settings read rejected"
			default:
				reply.Result = result
			}
		}
		_ = em.Notify("engine:settings-reply", reply)
	}()
}
