// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package jsonrpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func TestRelayRequestQueuesWritesInOrder(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	p := NewPeer(NewCodec(struct {
		io.Reader
		io.Writer
	}{strings.NewReader(""), w}))
	defer p.Close()
	const n = 32
	queued := make(chan struct{})
	responses := make(chan error, n)
	go func() {
		defer close(queued)
		for i := 0; i < n; i++ {
			params := json.RawMessage(`{"original":true}`)
			err := p.RelayRequest(fmt.Sprintf("relay-%d", i), params, func(_ json.RawMessage, _ *RPCError, err error) { responses <- err })
			if err != nil {
				responses <- err
			}
			// The asynchronous writer cannot retain the caller's buffer.
			copy(params, `{"original":null}`)
		}
	}()
	select {
	case <-queued:
	case <-time.After(time.Second):
		t.Fatal("RelayRequest blocked on worker stdin")
	}
	codec := NewCodec(struct {
		io.Reader
		io.Writer
	}{r, io.Discard})
	for i := 0; i < n; i++ {
		msg, err := codec.Read()
		if err != nil {
			t.Fatal(err)
		}
		if msg.Method != fmt.Sprintf("relay-%d", i) || string(msg.Params) != `{"original":true}` {
			t.Fatalf("reordered relay or retained params at %d: %+v", i, msg)
		}
		p.deliverResponse(&Message{ID: msg.ID, Result: json.RawMessage(`true`)})
	}
	for i := 0; i < n; i++ {
		select {
		case err := <-responses:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("relay did not respond")
		}
	}
}

func TestRelayRequestCloseCancelsBlockedAndQueuedWrites(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	p := NewPeer(NewCodec(struct {
		io.Reader
		io.Writer
	}{strings.NewReader(""), w}))
	defer p.Close()
	queued := make(chan struct{})
	responses := make(chan error, 2)
	go func() {
		defer close(queued)
		for i := 0; i < 2; i++ {
			if err := p.RelayRequest("blocked", nil, func(_ json.RawMessage, _ *RPCError, err error) { responses <- err }); err != nil {
				responses <- err
			}
		}
	}()
	select {
	case <-queued:
	case <-time.After(time.Second):
		t.Fatal("RelayRequest blocked before Close")
	}
	p.Close()
	for i := 0; i < 2; i++ {
		select {
		case err := <-responses:
			if !errors.Is(err, ErrPeerClosed) {
				t.Fatalf("error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Close leaked a pending relay")
		}
	}
	// Closing the owned transport releases the sole in-flight frame as well.
	_ = w.Close()
}

func TestRelayRequestWriteErrorReachesCallback(t *testing.T) {
	p := NewPeer(NewCodec(struct {
		io.Reader
		io.Writer
	}{strings.NewReader(""), &partialErrorWriter{}}))
	defer p.Close()
	response := make(chan error, 1)
	if err := p.RelayRequest("broken", nil, func(_ json.RawMessage, _ *RPCError, err error) { response <- err }); err != nil {
		t.Fatalf("accepted relay must report write errors via callback, got %v", err)
	}
	select {
	case err := <-response:
		if err == nil {
			t.Fatal("write failure lost")
		}
	case <-time.After(time.Second):
		t.Fatal("write failure leaked relay")
	}
}
