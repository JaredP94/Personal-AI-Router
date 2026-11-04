// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	settings "nvpair-shared/enginesettings"
)

func TestManagedPortCancelJoinsQueuedMove(t *testing.T) {
	e := newBundledExecutor(t, t.TempDir())
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	m := NewManager(NewCodec(server), e, nil)
	prepared := make(chan struct{})
	e.settingsParent = func(_ context.Context, method string, _ settings.Request, _ string) (json.RawMessage, error) {
		if method == "prepare-start" {
			close(prepared)
			return json.RawMessage(`"generation"`), nil
		}
		return json.RawMessage(`null`), nil
	}
	st, err := e.state("ollama")
	if err != nil {
		t.Fatal(err)
	}
	before := e.snapshot("ollama", st).Port
	st.lockOperation()
	locked := true
	defer func() {
		if locked {
			st.unlockOperation()
		}
	}()
	id, cancelID := json.RawMessage(`1`), json.RawMessage(`2`)
	m.handleMessage(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: "internal:set-port", Params: json.RawMessage(`{"engine":"ollama","port":11439,"operationId":"queued"}`)})
	<-prepared
	m.handleMessage(context.Background(), &Message{JSONRPC: "2.0", ID: &cancelID, Method: "internal:cancel-set-port", Params: json.RawMessage(`{"operationId":"queued"}`)})
	responses := make(chan *Message, 2)
	go func() {
		codec := NewCodec(client)
		for range 2 {
			msg, _ := codec.Read()
			responses <- msg
		}
	}()
	select {
	case msg := <-responses:
		t.Fatalf("cancellation acknowledged before queued operation joined: %+v", msg)
	case <-time.After(50 * time.Millisecond):
	}
	st.unlockOperation()
	locked = false
	for range 2 {
		select {
		case msg := <-responses:
			if msg == nil {
				t.Fatal("missing cancellation/result response")
			}
			if string(*msg.ID) == "1" && msg.Error == nil {
				t.Fatal("canceled move succeeded")
			}
			if string(*msg.ID) == "2" && msg.Error != nil {
				t.Fatal("cancellation failed")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("cancel did not join")
		}
	}
	if got := e.snapshot("ollama", st).Port; got != before {
		t.Fatalf("canceled move changed port to %d", got)
	}
}

func TestSetPortRejectsCanceledOrStaleQueuedMove(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprint("stale=", stale), func(t *testing.T) {
			dir := t.TempDir()
			e := newBundledExecutor(t, dir)
			st, err := e.state("ollama")
			if err != nil {
				t.Fatal(err)
			}
			before := e.snapshot("ollama", st).Port
			prepared := make(chan struct{})
			var revoked atomic.Bool
			e.settingsParent = func(_ context.Context, method string, _ settings.Request, _ string) (json.RawMessage, error) {
				if method == "prepare-start" {
					close(prepared)
					return json.RawMessage(`"generation"`), nil
				}
				if revoked.Load() {
					return nil, fmt.Errorf("revoked")
				}
				return json.RawMessage(`null`), nil
			}
			st.lockOperation()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := e.SetPort(ctx, "ollama", 11439); done <- err }()
			<-prepared
			if stale {
				revoked.Store(true)
			} else {
				cancel()
			}
			st.unlockOperation()
			select {
			case err := <-done:
				if err == nil {
					t.Error("invalidated queued move succeeded")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("queued move did not finish")
			}
			if got := e.snapshot("ollama", st).Port; got != before {
				t.Errorf("invalidated move changed port: %d -> %d", before, got)
			}
			if _, err := os.Stat(filepath.Join(dir, "ollama.json")); !os.IsNotExist(err) {
				t.Errorf("invalidated move persisted an override: %v", err)
			}
		})
	}
}
