// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package proxyport

import (
	"context"
	"encoding/json"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"nvpair-shared/jsonrpc"
)

func TestCancellationJoinsRegisteredProxyWrite(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	peer := jsonrpc.NewPeer(jsonrpc.NewCodec(client))
	go peer.Serve(nil, nil)
	codec := jsonrpc.NewCodec(server)
	var operations Operations
	var port atomic.Int32
	port.Store(11435)
	entered, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	go func() {
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			operations.Handle(msg, codec, func(ctx context.Context, newPort int) error {
				close(entered)
				<-ctx.Done()
				close(canceled)
				<-release // model an operation still leaving its listener critical section
				if err := ctx.Err(); err != nil {
					return err
				}
				port.Store(int32(newPort))
				return nil
			}, func(port int) any { return map[string]int{"port": port} })
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	writeDone := make(chan *jsonrpc.RPCError, 1)
	go func() {
		_, rpcErr, _ := peer.Call(ctx, "set-port", json.RawMessage(`{"port":11436,"operationId":"move"}`))
		writeDone <- rpcErr
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("write not registered")
	}
	joined := make(chan error, 1)
	go func() {
		_, _, err := peer.Call(ctx, "internal:cancel-set-port", json.RawMessage(`{"operationId":"move"}`))
		joined <- err
	}()
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("registered write did not observe cancellation")
	}
	select {
	case <-joined:
		t.Fatal("cancellation acknowledged before operation joined")
	default:
	}
	close(release)
	select {
	case err := <-joined:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("cancellation did not join")
	}
	select {
	case rpcErr := <-writeDone:
		if rpcErr == nil {
			t.Fatal("canceled write succeeded")
		}
	case <-ctx.Done():
		t.Fatal("write reply missing")
	}
	if port.Load() != 11435 {
		t.Fatalf("canceled write applied %d", port.Load())
	}
}
