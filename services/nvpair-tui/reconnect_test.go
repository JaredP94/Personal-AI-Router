// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"errors"
	"nvpair-tui/rpc"
	"testing"
)

type reconnectSession struct {
	runErr  error
	stopped bool
}

func (s *reconnectSession) Run() error { return s.runErr }
func (s *reconnectSession) Shutdown()  { s.stopped = true }
func TestOwnerReconnectsOnceAfterOrderlyShutdown(t *testing.T) {
	for _, secondErr := range []error{nil, rpc.ErrHeartbeatTimeout} {
		first := &reconnectSession{runErr: rpc.ErrHeartbeatTimeout}
		second := &reconnectSession{runErr: secondErr}
		calls := 0
		err := runOwnedSessions(context.Background(), func() (ownedSession, error) {
			calls++
			if calls == 1 {
				return first, nil
			}
			if !first.stopped {
				t.Fatal("new broker spawned before old shutdown")
			}
			return second, nil
		})
		if calls != 2 || !second.stopped {
			t.Fatal("unbounded reconnect or missing shutdown")
		}
		if !errors.Is(err, secondErr) {
			t.Fatalf("wrong reconnect outcome: %v", err)
		}
	}
	normal := &reconnectSession{}
	calls := 0
	if err := runOwnedSessions(context.Background(), func() (ownedSession, error) { calls++; return normal, nil }); err != nil || calls != 1 {
		t.Fatal("normal quit reconnected")
	}
}
