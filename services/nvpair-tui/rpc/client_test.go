// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"nvpair-shared/jsonrpc"
)

// newPair wires a Client to an in-memory server side over a full-duplex
// net.Pipe, runs the client read loop, and returns the client, a codec
// for the server end, and that end's connection (close it to simulate
// the broker disconnecting).
func newPair(t *testing.T) (*Client, *Codec, net.Conn) {
	t.Helper()
	c1, c2 := net.Pipe()
	client := NewClient(c1, c1)
	ctx, cancel := context.WithCancel(context.Background())
	go client.Run(ctx)
	t.Cleanup(func() {
		cancel()
		c1.Close()
		c2.Close()
	})
	return client, NewCodec(c2), c2
}

// TestCodecRoundTrip exercises the canonical codec end to end. The frame is a
// notification, so it carries the protocol v2 envelope and the read succeeds
// only because Notify stamped it — an unstamped frame of the same shape is a
// terminal *ProtocolMismatchError.
func TestCodecRoundTrip(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	a := NewCodec(c1)
	b := NewCodec(c2)

	go func() {
		_ = a.Notify("hello", json.RawMessage(`{"x":1}`))
	}()

	msg, err := b.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !msg.IsNotification() || msg.Method != "hello" {
		t.Fatalf("unexpected frame: %+v", msg)
	}
	if string(msg.Params) != `{"x":1}` {
		t.Fatalf("params = %s", msg.Params)
	}
	if msg.Epoch == nil || *msg.Epoch != jsonrpc.Epoch() {
		t.Errorf("epoch = %v, want %s", msg.Epoch, jsonrpc.Epoch())
	}
	if msg.Seq == nil || *msg.Seq != 1 {
		t.Errorf("seq = %v, want 1", msg.Seq)
	}
}

func TestClientCallMatchesResponse(t *testing.T) {
	client, server, _ := newPair(t)

	// Server: read the request and echo a result tagged with its id.
	go func() {
		req, err := server.Read()
		if err != nil {
			return
		}
		_ = server.Respond(req.ID, json.RawMessage(`{"pong":true}`))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := client.Call(ctx, "ping", map[string]string{"a": "b"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if string(resp.Result) != `{"pong":true}` {
		t.Fatalf("result = %s", resp.Result)
	}
}

// TestClientRequestCarriesNoStamp pins the other half of the envelope rule: a
// request must NOT carry epoch/seq, because jsonrpc.validate() rejects a stamp
// on any frame that is not a notification. The client's own reader enforces it,
// so this is checked by reading the request back through the canonical codec.
func TestClientRequestCarriesNoStamp(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	client := NewClient(c1, c1)
	server := NewCodec(c2)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = client.Call(ctx, "ping", nil)
	}()

	req, err := server.Read()
	if err != nil {
		t.Fatalf("read request: %v", err)
	}
	if !req.IsRequest() {
		t.Fatalf("expected a request, got %+v", req)
	}
	if req.Epoch != nil || req.Seq != nil {
		t.Errorf("request must not be stamped: epoch=%v seq=%v", req.Epoch, req.Seq)
	}
}

func TestClientCallSurfacesRPCError(t *testing.T) {
	client, server, _ := newPair(t)
	go func() {
		req, err := server.Read()
		if err != nil {
			return
		}
		_ = server.RespondError(req.ID, -32000, "boom")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := client.Call(ctx, "explode", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if rpcErr, ok := err.(*RPCError); !ok || rpcErr.Code != -32000 {
		t.Fatalf("expected *RPCError -32000, got %v", err)
	}
}

func TestClientDeliversNotifications(t *testing.T) {
	client, server, _ := newPair(t)
	go func() {
		_ = server.Notify("errors:update", json.RawMessage(`[]`))
	}()

	select {
	case msg, ok := <-client.Notifications():
		if !ok {
			t.Fatal("notifications channel closed")
		}
		if msg.Method != "errors:update" {
			t.Fatalf("method = %s", msg.Method)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for notification")
	}
}

func TestClientDisconnectClosesNotifications(t *testing.T) {
	client, _, srv := newPair(t)
	srv.Close() // broker drops the connection -> client read loop hits EOF

	done := make(chan struct{})
	go func() {
		// Drain until the channel closes; a closed channel yields the
		// zero value with ok=false.
		for range client.Notifications() {
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("notifications channel was not closed after disconnect")
	}
}

// TestClientNotifyWritesInSeqOrderUnderConcurrency pins the wire-order contract
// for the TUI's only notification send path: the seq the broker reads must equal
// the frame's position on the wire, with no gaps and no repeats. The codec
// stamps seq under the same write lock that puts the frame on the wire, so wire
// order equals seq order; stamping outside that lock would break the documented
// per-connection ordering guarantee. (A receiver never resyncs on a seq gap; only
// an epoch change starts recovery.)
//
// Run with -count=20: the interleaving that breaks an unstamped implementation
// is rare, so a single green run proves little.
func TestClientNotifyWritesInSeqOrderUnderConcurrency(t *testing.T) {
	const (
		senders     = 16
		perSender   = 8
		totalFrames = senders * perSender
	)

	var sink lockedBuffer
	client := NewClient(bytes.NewReader(nil), &sink)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for s := range senders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release every sender at once to maximise overlap
			for i := range perSender {
				if err := client.Notify("workload:state-changed", map[string]int{"s": s, "i": i}); err != nil {
					t.Errorf("notify: %v", err)
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	reader := jsonrpc.NewCodec(readWriter{Reader: bytes.NewReader(sink.Bytes()), Writer: io.Discard})
	for want := uint64(1); want <= totalFrames; want++ {
		msg, err := reader.Read()
		if err != nil {
			t.Fatalf("reading the frame stamped seq %d: %v", want, err)
		}
		if msg.Seq == nil || *msg.Seq != want {
			t.Fatalf("wire order != seq order: the frame at wire position %d carries seq %v", want, msg.Seq)
		}
		if msg.Epoch == nil || *msg.Epoch != jsonrpc.Epoch() {
			t.Fatalf("frame with seq %d carries the wrong epoch", want)
		}
	}
	if _, err := reader.Read(); err != io.EOF {
		t.Fatalf("expected exactly %d frames then EOF, got err %v", totalFrames, err)
	}
}

// lockedBuffer is safe to hand to a codec several goroutines notify through. The
// codec serializes its own writes, but the test does not assume that and the
// buffer is cheap to make safe.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

// TestEpochNotificationStillReachesTheNotificationChannel pins that broker:epoch
// is delivered on the notification stream like any other frame, with its
// envelope epoch intact: the UI learns the epoch from these envelopes.
func TestEpochNotificationStillReachesTheNotificationChannel(t *testing.T) {
	client, server, _ := newPair(t)

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- server.Notify(BrokerEpochMethod, json.RawMessage(`{}`))
	}()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case err := <-writeDone:
			if err != nil {
				t.Fatalf("write broker:epoch: %v", err)
			}
			writeDone = nil
		case msg, ok := <-client.Notifications():
			if !ok {
				t.Fatal("notification channel closed before broker:epoch arrived")
			}
			if msg.Method != BrokerEpochMethod {
				t.Fatalf("method = %s, want %s", msg.Method, BrokerEpochMethod)
			}
			if msg.Epoch == nil || *msg.Epoch != jsonrpc.Epoch() {
				t.Fatalf("broker:epoch envelope epoch = %v, want this process's epoch %q", msg.Epoch, jsonrpc.Epoch())
			}
			return
		case <-deadline:
			t.Fatal("broker:epoch never reached the notification channel")
		}
	}
}
