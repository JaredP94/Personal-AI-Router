// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// Only this read is relayed upward; the worker cannot mutate node settings.
const lanBindSettingMethod = "settings/get-engine-allow-lan-bind"

type settingsReply struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error,omitempty"`
}

type settingsRelay struct {
	mu      sync.Mutex
	pending map[string]chan settingsReply
	send    func(string, any) error
}

// lanBindAllowed reads through on each non-loopback override. Missing settings,
// malformed values, transport failures and timeouts all deny permission.
func (m *Manager) lanBindAllowed(parent context.Context) bool {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	if ctx.Err() != nil {
		return false
	}
	r := &m.settingsRelay
	id := newOpID()
	ch := make(chan settingsReply, 1)
	r.mu.Lock()
	if len(r.pending) >= 64 {
		r.mu.Unlock()
		return false
	}
	if r.pending == nil {
		r.pending = make(map[string]chan settingsReply)
	}
	r.pending[id] = ch
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.pending, id); r.mu.Unlock() }()
	if err := r.send("engine:settings-request", map[string]string{"id": id, "method": lanBindSettingMethod}); err != nil {
		return false
	}
	select {
	case reply := <-ch:
		var result struct {
			Value *bool `json:"value"`
		}
		return reply.Error == "" && json.Unmarshal(reply.Result, &result) == nil && result.Value != nil && *result.Value
	case <-ctx.Done():
		return false
	}
}

func (r *settingsRelay) reply(raw json.RawMessage) {
	var reply settingsReply
	if json.Unmarshal(raw, &reply) != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if ch := r.pending[reply.ID]; ch != nil {
		select {
		case ch <- reply:
		default:
		}
	}
}
