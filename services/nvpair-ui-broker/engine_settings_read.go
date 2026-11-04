// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	settings "nvpair-shared/enginesettings"
)

// handleEngineSettingsRead demultiplexes the live bind-permission read and
// engine settings operations through one captured worker and reply path.
func (b *Broker) handleEngineSettingsRead(raw json.RawMessage) {
	var relay settings.Relay
	if json.Unmarshal(raw, &relay) != nil || relay.ID == "" {
		return
	}
	worker := b.getEngineMgr()
	if worker == nil {
		return
	}
	budget := settings.RelayBudget
	if relay.Method == "settings/get-engine-allow-lan-bind" {
		budget = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	b.settingsRelayMu.Lock()
	if b.settingsCancels == nil {
		b.settingsCancels = make(map[string]context.CancelFunc)
	}
	if len(b.settingsCancels) >= 128 {
		b.settingsRelayMu.Unlock()
		cancel()
		_ = worker.Notify("engine:settings-reply", map[string]string{"id": relay.ID, "error": "too many settings requests"})
		return
	}
	b.settingsCancels[relay.ID] = cancel
	b.settingsRelayMu.Unlock()
	go func() {
		defer cancel()
		defer func() { b.settingsRelayMu.Lock(); delete(b.settingsCancels, relay.ID); b.settingsRelayMu.Unlock() }()
		var result any
		var err error
		if relay.Method == "settings/get-engine-allow-lan-bind" {
			nodeSettings := b.getSettings()
			if nodeSettings == nil {
				err = fmt.Errorf("node-settings unavailable")
			} else {
				var rpcErr *RPCError
				result, rpcErr, err = nodeSettings.Call(ctx, relay.Method, nil)
				if rpcErr != nil {
					err = fmt.Errorf("node-settings read rejected")
				}
			}
		} else if relay.Method == "prepare-start" {
			result, err = b.prepareEngineStart(ctx, relay.Request.Engine, relay.Request.RequestID)
		} else if relay.Method == "authorize-start" {
			err = b.authorizeEngineStart(relay.Request)
		} else if relay.Method == "rebind" || relay.Method == "rollback" {
			b.settingsRelayMu.Lock()
			operation, ok := b.activeSettings[relay.Request.RequestID]
			if ok && operation.Callbacks != nil {
				operation.Callbacks.Add(1)
				defer operation.Callbacks.Done()
			}
			b.settingsRelayMu.Unlock()
			if !ok || operation.Engine != relay.Request.Engine || operation.Port != relay.Request.Settings.ProxyPort {
				err = fmt.Errorf("unknown settings operation")
			} else {
				port := operation.Port
				if relay.Method == "rollback" {
					// Only the broker's captured previous port is authorized;
					// the worker cannot nominate an arbitrary recovery binding.
					port = operation.PreviousPort
					b.settingsStartupPort(operation.Engine, port)
				}
				err = b.rebindSettingsProxy(ctx, operation.Engine, port)
			}
		} else {
			result, err = b.dispatchEngineSettings(ctx, relay.Method, relay.Request, relay.Caller)
		}
		reply := struct {
			ID     string `json:"id"`
			Result any    `json:"result"`
			Error  string `json:"error,omitempty"`
		}{ID: relay.ID, Result: result}
		if err != nil {
			reply.Error = err.Error()
		}
		_ = worker.Notify("engine:settings-reply", reply)
	}()
}
