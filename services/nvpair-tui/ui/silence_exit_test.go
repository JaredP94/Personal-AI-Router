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
	"time"
)

type expiredSession struct{}

func (expiredSession) Read([]byte) (int, error) { return 0, rpc.ErrHeartbeatTimeout }
func TestSilenceLossExitsRootWithDistinctOwnerResult(t *testing.T) {
	c := rpc.NewClient(expiredSession{}, io.Discard)
	_ = c.Run(context.Background())
	m := New(c, nil, defaultViews(c))
	final, cmd := m.Update(waitForNotification(c)())
	result, ok := final.(Model)
	if !ok || !result.disconnected || !errors.Is(result.connectionLoss, rpc.ErrHeartbeatTimeout) {
		t.Fatalf("connection loss not delivered to owner: %#v", final)
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("root did not exit on silence")
	}
}

// A setup that fails because the session already expired must still deliver
// the silence loss to the owner, which only happens if notifications are
// consumed after the failure.
func TestSetupFailureStillDeliversSilenceLoss(t *testing.T) {
	c := rpc.NewClient(expiredSession{}, io.Discard)
	_ = c.Run(context.Background())
	l := newLoop(New(c, nil, defaultViews(c)))
	l.exec(l.m.setupConnection())
	deadline := time.After(2 * time.Second)
	for {
		select {
		case msg := <-l.inbox:
			if _, ok := msg.(tea.QuitMsg); ok {
				if !errors.Is(l.m.connectionLoss, rpc.ErrHeartbeatTimeout) {
					t.Fatalf("quit without silence loss: %v", l.m.connectionLoss)
				}
				return
			}
			l.turn(t, msg)
		case <-deadline:
			t.Fatal("setup failure left the model deaf to the silence loss")
		}
	}
}
