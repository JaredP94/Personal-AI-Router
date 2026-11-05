// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	settings "nvpair-shared/enginesettings"
)

// Profile mutations are atomic, persistence-only engine settings operations.
// Capture the worker and retain no settings/lifecycle lock across its call.
// CallNoTimeout removes timed-out correlations, fencing late responses.
func (b *Broker) relayLoadProfile(ctx context.Context, method string, p settings.Request) (json.RawMessage, error) {
	if p.Engine != "lmstudio" {
		return nil, errors.New("Load options are only available for LM Studio.")
	}
	if method != "get-load-profile" && method != "preview-load-profile" && method != "apply-load-profile" {
		return nil, errors.New("Unsupported load options method.")
	}
	ctx, cancel := context.WithTimeout(ctx, settings.LoadProfileRelayBudget)
	defer cancel()
	if ctx.Err() != nil {
		return nil, errors.New("Load options request canceled.")
	}
	worker := b.getEngineMgr()
	if worker == nil {
		return nil, errors.New("Engine manager unavailable.")
	}
	rpcMethod := "engine:" + method
	nodeID := b.nodeID
	if p.NodeID != "" && p.NodeID != b.nodeID {
		rpcMethod = "engine:remote-" + method
		nodeID = p.NodeID
	}
	result, rpcErr, err := worker.CallNoTimeout(ctx, rpcMethod, settingsJSON(p))
	if err != nil || ctx.Err() != nil || b.getEngineMgr() != worker {
		return nil, errors.New("Load options request failed. Reload before retrying.")
	}
	if rpcErr != nil {
		return nil, errors.New(rpcErr.Message)
	}
	if method != "preview-load-profile" {
		var snap settings.LoadProfileSnapshot
		if json.Unmarshal(result, &snap) != nil || snap.Engine != "lmstudio" || snap.Model != p.Model {
			return nil, errors.New("Invalid load options response.")
		}
		snap.NodeID = nodeID
		return json.Marshal(snap)
	}
	return result, nil
}
func isLMStudioModelMutation(method string, raw json.RawMessage) bool {
	var p struct {
		Engine string `json:"engine"`
		Action string `json:"action"`
	}
	if json.Unmarshal(raw, &p) != nil || p.Engine != "lmstudio" {
		return false
	}
	if method == "engine:action" {
		return p.Action == "load_model" || p.Action == "unload_model" || p.Action == "delete_model"
	}
	return method == "engine:remote-load-model" || method == "engine:remote-unload-model" || method == "engine:remote-delete-model"
}

func (b *Broker) handleLoadProfile(msg *Message) {
	var p settings.Request
	if json.Unmarshal(msg.Params, &p) != nil {
		_ = b.codec.RespondError(msg.ID, -32602, "Invalid load options request.")
		return
	}
	result, err := b.relayLoadProfile(context.Background(), strings.TrimPrefix(msg.Method, "engine:"), p)
	if err != nil {
		_ = b.codec.RespondError(msg.ID, -32000, err.Error())
		return
	}
	_ = b.codec.Respond(msg.ID, result)
}
