// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "context"

// Managed port moves have a broker-chosen correlation ID. Cancellation is a
// join, not a fire-and-forget hint: after its response no write from that move
// can occur, including one queued on opMu or a slow prepare-start relay.
type managedPortParam struct {
	Engine      string `json:"engine"`
	Port        int    `json:"port"`
	OperationID string `json:"operationId"`
}

type managedPortOperationKey struct{}

type managedPortMove struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (m *Manager) startManagedPort(ctx context.Context, msg *Message) {
	var p managedPortParam
	if !m.parse(msg, &p) {
		return
	}
	if p.Engine == "" || p.Port < 1 || p.Port > 65535 || p.OperationID == "" {
		_ = m.codec.RespondError(msg.ID, -32602, "engine, port and operationId are required")
		return
	}
	ctx, cancel := context.WithCancel(context.WithValue(ctx, managedPortOperationKey{}, p.OperationID))
	move := &managedPortMove{cancel: cancel, done: make(chan struct{})}
	m.managedPortsMu.Lock()
	if m.managedPorts == nil {
		m.managedPorts = make(map[string]*managedPortMove)
	}
	if m.managedPorts[p.OperationID] != nil || len(m.managedPorts) >= 64 {
		m.managedPortsMu.Unlock()
		cancel()
		_ = m.codec.RespondError(msg.ID, -32000, "managed port operation unavailable")
		return
	}
	// Register synchronously on the reader, before a following cancel frame.
	m.managedPorts[p.OperationID] = move
	m.managedPortsMu.Unlock()
	go func() {
		st, err := m.exec.SetPort(ctx, p.Engine, p.Port)
		cancel()
		m.managedPortsMu.Lock()
		delete(m.managedPorts, p.OperationID)
		close(move.done)
		m.managedPortsMu.Unlock()
		m.respondOrErr(msg, st, err)
	}()
}

func (m *Manager) cancelManagedPort(msg *Message) {
	var p struct {
		OperationID string `json:"operationId"`
	}
	if !m.parse(msg, &p) {
		return
	}
	if p.OperationID == "" {
		_ = m.codec.RespondError(msg.ID, -32602, "operationId is required")
		return
	}
	m.managedPortsMu.Lock()
	move := m.managedPorts[p.OperationID]
	if move != nil {
		move.cancel()
	}
	m.managedPortsMu.Unlock()
	go func() {
		if move != nil {
			<-move.done
		}
		_ = m.codec.Respond(msg.ID, nil)
	}()
}
