// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package ui

import (
	"context"
	"errors"
	tea "github.com/charmbracelet/bubbletea"
	"io"
	"nvpair-tui/rpc"
	"testing"
)

type expiredSession struct{}

func (expiredSession) Read([]byte) (int, error) { return 0, rpc.ErrHeartbeatTimeout }
func TestSilenceLossExitsRootWithDistinctOwnerResult(t *testing.T) {
	c := rpc.NewClient(expiredSession{}, io.Discard)
	_ = c.Run(context.Background())
	m := New(c, nil, nil, defaultViews(c))
	final, cmd := m.Update(waitForNotification(c)())
	result, ok := final.(Model)
	if !ok || !result.disconnected || !errors.Is(result.connectionLoss, rpc.ErrHeartbeatTimeout) {
		t.Fatalf("connection loss not delivered to owner: %#v", final)
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("root did not exit on silence")
	}
}
