// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package jsonrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// pipePeers wires two Peers over a synchronous in-memory net.Pipe. Handlers
// that write a reply do so in a goroutine so a blocking write on the
// unbuffered pipe never stalls the read pump (real stdio pipes are buffered).
func pipePeers(t *testing.T) (a, b *Peer) {
	t.Helper()
	c1, c2 := net.Pipe()
	a = NewPeer(NewCodec(c1))
	b = NewPeer(NewCodec(c2))
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
	return a, b
}

func TestPeerCallReturnsResult(t *testing.T) {
	a, b := pipePeers(t)
	go b.Serve(func(req *Message) {
		go b.Respond(req.ID, map[string]string{"echo": req.Method})
	}, nil)
	go a.Serve(nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, rpcErr, err := a.Call(ctx, "ping", json.RawMessage(`{"x":1}`))
	if err != nil || rpcErr != nil {
		t.Fatalf("Call err=%v rpcErr=%v", err, rpcErr)
	}
	var out map[string]string
	if json.Unmarshal(res, &out); out["echo"] != "ping" {
		t.Fatalf("unexpected result: %s", res)
	}
}

func TestPeerCallReturnsRPCError(t *testing.T) {
	a, b := pipePeers(t)
	go b.Serve(func(req *Message) {
		go b.RespondError(req.ID, -32601, "method not found")
	}, nil)
	go a.Serve(nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, rpcErr, err := a.Call(ctx, "nope", nil)
	if err != nil {
		t.Fatalf("transport err: %v", err)
	}
	if rpcErr == nil || rpcErr.Code != -32601 {
		t.Fatalf("want rpc error -32601, got %+v", rpcErr)
	}
}

func TestPeerSimultaneousInboundRequest(t *testing.T) {
	// Both peers Call each other while both are serving inbound requests. The
	// read pump is separate from Call, so neither side deadlocks.
	a, b := pipePeers(t)
	handler := func(self *Peer) func(*Message) {
		return func(req *Message) { go self.Respond(req.ID, map[string]string{"from": req.Method}) }
	}
	go a.Serve(handler(a), nil)
	go b.Serve(handler(b), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	type res struct {
		raw json.RawMessage
		err error
	}
	ares := make(chan res, 1)
	bres := make(chan res, 1)
	go func() { r, _, e := a.Call(ctx, "a-calls-b", nil); ares <- res{r, e} }()
	go func() { r, _, e := b.Call(ctx, "b-calls-a", nil); bres <- res{r, e} }()

	for _, ch := range []chan res{ares, bres} {
		select {
		case r := <-ch:
			if r.err != nil {
				t.Fatalf("simultaneous call failed: %v", r.err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("simultaneous calls deadlocked")
		}
	}
}

func TestPeerNotificationOrdering(t *testing.T) {
	a, b := pipePeers(t)
	got := make(chan string, 16)
	go b.Serve(nil, func(method string, _ json.RawMessage) { got <- method })
	go a.Serve(nil, nil)

	const n = 8
	for i := 0; i < n; i++ {
		if err := a.Notify(fmt.Sprintf("n%d", i), nil); err != nil {
			t.Fatalf("Notify: %v", err)
		}
	}
	for i := 0; i < n; i++ {
		select {
		case m := <-got:
			if m != fmt.Sprintf("n%d", i) {
				t.Fatalf("out of order: got %s want n%d", m, i)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for notification %d", i)
		}
	}
}

func TestPeerCallCtxCancel(t *testing.T) {
	a, b := pipePeers(t)
	go b.Serve(func(*Message) {}, nil) // never responds
	go a.Serve(nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	_, _, err := a.Call(ctx, "hang", nil)
	if err != context.Canceled {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestCallDeadlineWithBlockedWriter(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	peer := NewPeer(writeCodec(writer))
	defer peer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := peer.Call(ctx, "worker/health", nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err != context.DeadlineExceeded {
			t.Fatalf("Call error = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Call did not honour its deadline while the request write was blocked")
	}
	peer.mu.Lock()
	pending := len(peer.pending)
	peer.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending requests after deadline = %d, want 0", pending)
	}
}

func TestCallWithCanceledContextDoesNotWrite(t *testing.T) {
	var buf bytes.Buffer
	peer := NewPeer(writeCodec(&buf))
	defer peer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := peer.Call(ctx, "must-not-send", nil); err != context.Canceled {
		t.Fatalf("Call error = %v, want context.Canceled", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("canceled Call wrote a request: %s", buf.String())
	}
}

func TestCallCanceledWhileWaitingForWriterDoesNotWrite(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	peer := NewPeer(writeCodec(writer))
	defer peer.Close()
	first := make(chan error, 1)
	go func() { first <- peer.Notify("first", nil) }()
	prefix := make([]byte, 1)
	if _, err := io.ReadFull(reader, prefix); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := peer.Call(ctx, "must-not-send", nil); done <- err }()
	select {
	case err := <-done:
		if err != context.DeadlineExceeded {
			t.Fatalf("Call error = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Call blocked waiting for another writer")
	}

	codec := readCodec(io.MultiReader(bytes.NewReader(prefix), reader))
	msg, err := codec.Read()
	if err != nil || msg.Method != "first" {
		t.Fatalf("first frame = %+v, error = %v", msg, err)
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	// Other calls remain usable after a queued call expires.
	go func() { _, _, err := peer.Call(context.Background(), "next", nil); done <- err }()
	msg, err = codec.Read()
	if err != nil || msg.Method != "next" {
		t.Fatalf("frame after canceled Call = %+v, error = %v", msg, err)
	}
	peer.Close()
	if err := <-done; err != ErrPeerClosed {
		t.Fatalf("live Call error = %v, want ErrPeerClosed", err)
	}
}

// The request writer itself must exit on cancellation, not just the Call
// waiting for it. Otherwise repeated probes retain goroutines and params behind
// a single stuck frame even though each Call returns on time.
func TestCanceledQueuedRequestWritersExit(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	peer := NewPeer(writeCodec(writer))
	defer peer.Close()
	first := make(chan error, 1)
	go func() { first <- peer.Notify("first", nil) }()
	prefix := make([]byte, 1)
	if _, err := io.ReadFull(reader, prefix); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	const queued = 16
	exited := make(chan error, queued)
	for i := range queued {
		go func() {
			// Exercise the same writer path as Call, but observe its exit
			// directly rather than Call's independent context select.
			exited <- peer.writeRequest(ctx, int64(i+1), "must-not-send", json.RawMessage(`{"probe":true}`))
		}()
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for i := range queued {
		select {
		case err := <-exited:
			if err != context.DeadlineExceeded {
				t.Fatalf("queued writer error = %v, want context.DeadlineExceeded", err)
			}
		case <-deadline.C:
			t.Fatalf("only %d/%d canceled queued writers exited while the first frame was blocked", i, queued)
		}
	}
	select {
	case err := <-first:
		t.Fatalf("first writer finished before its frame was drained: %v", err)
	default:
	}

	// Cancellation must not release the active writer's lock or damage its
	// frame. Let it finish only after every canceled queued writer has exited.
	msg, err := readCodec(io.MultiReader(bytes.NewReader(prefix), reader)).Read()
	if err != nil || msg.Method != "first" {
		t.Fatalf("first frame = %+v, error = %v", msg, err)
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}

func TestCallDeadlineKeepsPartialFrameSerialized(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	peer := NewPeer(writeCodec(writer))
	defer peer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := peer.Call(ctx, "first", nil); done <- err }()
	prefix := make([]byte, 1)
	if _, err := io.ReadFull(reader, prefix); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != context.DeadlineExceeded {
			t.Fatalf("Call error = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Call did not return after partially writing its frame")
	}

	go func() { done <- peer.Notify("second", nil) }()
	codec := readCodec(io.MultiReader(bytes.NewReader(prefix), reader))
	for _, method := range []string{"first", "second"} {
		msg, err := codec.Read()
		if err != nil || msg.Method != method {
			t.Fatalf("want complete %q frame, got %+v, error = %v", method, msg, err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCallBlockedWriteFinishesWhenWriterCloses(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	peer := NewPeer(writeCodec(writer))
	defer peer.Close()
	done := make(chan error, 1)
	go func() { _, _, err := peer.Call(context.Background(), "blocked", nil); done <- err }()
	prefix := make([]byte, 1)
	if _, err := io.ReadFull(reader, prefix); err != nil {
		t.Fatal(err)
	}
	peer.Close()
	select {
	case err := <-done:
		if err != ErrPeerClosed {
			t.Fatalf("Call error = %v, want ErrPeerClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not release Call with a blocked write")
	}
	writer.Close()
	// Acquiring the codec lock proves the abandoned write has finished.
	go func() { peer.codec.wmu <- struct{}{}; <-peer.codec.wmu; done <- nil }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("closing the writer did not release the codec lock")
	}
}

// partialErrorWriter leaves a truncated frame on its first write but would
// accept later bytes, exposing a codec that reuses a damaged stream.
type partialErrorWriter struct {
	buf bytes.Buffer
}

func (w *partialErrorWriter) Write(data []byte) (int, error) {
	if w.buf.Len() == 0 {
		n, _ := w.buf.Write(data[:len(data)/2])
		return n, io.ErrUnexpectedEOF
	}
	return w.buf.Write(data)
}

func TestCallWriteErrorPreventsAppendingToPartialFrame(t *testing.T) {
	var writer partialErrorWriter
	peer := NewPeer(writeCodec(&writer))
	defer peer.Close()
	if _, _, err := peer.Call(context.Background(), "first", nil); err != io.ErrUnexpectedEOF {
		t.Fatalf("Call error = %v, want io.ErrUnexpectedEOF", err)
	}
	prefix := writer.buf.String()
	id := json.RawMessage(`7`)
	for _, write := range []func() error{
		func() error { return peer.Notify("later", nil) },
		func() error { return peer.Respond(&id, true) },
		func() error { return peer.codec.Request(&id, "later", nil) },
	} {
		if err := write(); err == nil {
			t.Error("write to a damaged stream succeeded")
		}
	}
	if writer.buf.String() != prefix {
		t.Fatalf("later frames appended to a truncated frame: %q", writer.buf.String())
	}
}

func TestPeerCloseWakesPendingCall(t *testing.T) {
	c1, c2 := net.Pipe()
	a := NewPeer(NewCodec(c1))
	b := NewPeer(NewCodec(c2))
	go b.Serve(func(*Message) {}, nil) // never responds
	go a.Serve(nil, nil)

	errc := make(chan error, 1)
	go func() { _, _, e := a.Call(context.Background(), "hang", nil); errc <- e }()
	time.Sleep(50 * time.Millisecond)
	_ = c1.Close() // a's read pump errors -> a.Close() -> pending Call wakes
	_ = c2.Close()

	select {
	case e := <-errc:
		if e != ErrPeerClosed {
			t.Fatalf("want ErrPeerClosed, got %v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending Call was not woken on close")
	}
}

func TestPeerCallAfterCloseFailsFast(t *testing.T) {
	a, _ := pipePeers(t)
	a.Close()
	if _, _, err := a.Call(context.Background(), "m", nil); err != ErrPeerClosed {
		t.Fatalf("want ErrPeerClosed after Close, got %v", err)
	}
}

func TestPeerRelayRequest(t *testing.T) {
	a, b := pipePeers(t)
	go b.Serve(func(req *Message) {
		go b.Respond(req.ID, map[string]int{"ok": 1})
	}, nil)
	go a.Serve(nil, nil)

	type relayed struct {
		res json.RawMessage
		err error
	}
	done := make(chan relayed, 1)
	if err := a.RelayRequest("m", nil, func(res json.RawMessage, _ *RPCError, err error) {
		done <- relayed{res, err}
	}); err != nil {
		t.Fatalf("RelayRequest: %v", err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("relay err: %v", r.err)
		}
		var out map[string]int
		if json.Unmarshal(r.res, &out); out["ok"] != 1 {
			t.Fatalf("unexpected relay result: %s", r.res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relay respond not invoked")
	}
}
