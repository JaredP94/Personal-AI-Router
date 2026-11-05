// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package proxyport fences broker-owned proxy rebinds by operation ID.
package proxyport

import (
	"context"
	"encoding/json"
	"sync"

	"nvpair-shared/jsonrpc"
)

type operation struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Operations registers writes on the reader before dispatch, so a following
// cancellation cannot overtake registration. Its zero value is ready to use.
type Operations struct {
	mu      sync.Mutex
	pending map[string]*operation
}

// Handle consumes port requests synchronously on the reader, while running
// listener work and cancellation joins off-reader so replies can keep flowing.
func (o *Operations) Handle(msg *jsonrpc.Message, codec *jsonrpc.Codec, apply func(context.Context, int) error, ready func(int) any) bool {
	switch msg.Method {
	case "set-port":
		var p struct {
			Port        int    `json:"port"`
			OperationID string `json:"operationId"`
		}
		if json.Unmarshal(msg.Params, &p) != nil {
			_ = codec.RespondError(msg.ID, -32602, "invalid params: expected {\"port\": <int>}")
			return true
		}
		if p.Port < 1 || p.Port > 65535 {
			_ = codec.RespondError(msg.ID, -32602, "port must be between 1 and 65535")
			return true
		}
		ctx, cancel := context.WithCancel(context.Background())
		op := &operation{cancel: cancel, done: make(chan struct{})}
		o.mu.Lock()
		if o.pending == nil {
			o.pending = make(map[string]*operation)
		}
		if o.pending[p.OperationID] != nil || len(o.pending) >= 64 {
			o.mu.Unlock()
			cancel()
			_ = codec.RespondError(msg.ID, -32000, "proxy port operation unavailable")
			return true
		}
		o.pending[p.OperationID] = op
		o.mu.Unlock()
		go func() {
			err := apply(ctx, p.Port)
			cancel()
			o.mu.Lock()
			delete(o.pending, p.OperationID)
			close(op.done)
			o.mu.Unlock()
			if err != nil {
				_ = codec.RespondError(msg.ID, -32000, err.Error())
			} else {
				_ = codec.Respond(msg.ID, ready(p.Port))
			}
		}()
		return true
	case "internal:cancel-set-port":
		var p struct {
			OperationID string `json:"operationId"`
		}
		if json.Unmarshal(msg.Params, &p) != nil || p.OperationID == "" {
			_ = codec.RespondError(msg.ID, -32602, "operationId is required")
			return true
		}
		o.mu.Lock()
		op := o.pending[p.OperationID]
		if op != nil {
			op.cancel()
		}
		o.mu.Unlock()
		go func() {
			if op != nil {
				<-op.done
			}
			_ = codec.Respond(msg.ID, nil)
		}()
		return true
	}
	return false
}
