// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
// the frame's position on the wire, with no gaps and no repeats. §2.7 gap
// detection treats an inversion as a dropped frame and answers with a spurious
// resync, so stamping outside the codec's write lock would be a real defect.
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

// writeRawNotif puts one hand-built notification on the wire with an explicit
// epoch and seq. The server end's Codec.Notify cannot be used here because it
// stamps with THIS process's epoch, and the whole point of these tests is to
// deliver an epoch that is NOT the one the process already holds.
func writeRawNotif(t *testing.T, conn net.Conn, method, epoch string, seq int) {
	t.Helper()
	frame := fmt.Sprintf(
		`{"jsonrpc":"2.0","method":%q,"params":{},"epoch":%q,"seq":%d}`+"\n",
		method, epoch, seq)
	go func() {
		_, _ = io.WriteString(conn, frame)
	}()
}

// newEpochRecorder wires a client with a change callback that reports epochs on
// a buffered channel, so a test can count callbacks instead of racing a shared
// counter.
func newEpochRecorder(t *testing.T) (*Client, net.Conn, <-chan string) {
	t.Helper()
	client, _, conn := newPair(t)
	changes := make(chan string, 16)
	client.OnEpochChange(func(epoch string) { changes <- epoch })
	return client, conn, changes
}

// awaitChange waits for one epoch-change callback, or fails.
func awaitChange(t *testing.T, changes <-chan string) string {
	t.Helper()
	select {
	case epoch := <-changes:
		return epoch
	case <-time.After(3 * time.Second):
		t.Fatal("no onEpochChange callback within 3s")
		return ""
	}
}

// assertNoChange fails if any epoch-change callback arrives within a short
// settle window. The window is short because the callback is driven by a frame
// that has already been written synchronously — there is no timer to wait on.
func assertNoChange(t *testing.T, changes <-chan string) {
	t.Helper()
	select {
	case epoch := <-changes:
		t.Fatalf("onEpochChange fired with %q; a notification that does not change the epoch must be silent", epoch)
	case <-time.After(250 * time.Millisecond):
	}
}

// TestEpochChangeFiresCallbackExactlyOnce is the client half of the broker:epoch
// contract: the first notification of a connection learns the epoch, further
// frames carrying that SAME epoch stay silent, and a genuinely different epoch
// fires exactly one more time.
func TestEpochChangeFiresCallbackExactlyOnce(t *testing.T) {
	client, conn, changes := newEpochRecorder(t)
	if got := client.epoch(); got != "" {
		t.Fatalf("epoch() = %q before any frame, want empty", got)
	}

	writeRawNotif(t, conn, BrokerEpochMethod, "epoch-A", 1)
	if got := awaitChange(t, changes); got != "epoch-A" {
		t.Fatalf("first callback epoch = %q, want epoch-A", got)
	}
	if got := client.epoch(); got != "epoch-A" {
		t.Fatalf("epoch() = %q after the first frame, want epoch-A", got)
	}

	// Same epoch again: the ordinary heartbeat stream must not re-fire.
	writeRawNotif(t, conn, "heartbeat", "epoch-A", 2)
	assertNoChange(t, changes)

	// A different epoch fires again, exactly once.
	writeRawNotif(t, conn, BrokerEpochMethod, "epoch-B", 3)
	if got := awaitChange(t, changes); got != "epoch-B" {
		t.Fatalf("second callback epoch = %q, want epoch-B", got)
	}
	writeRawNotif(t, conn, "heartbeat", "epoch-B", 4)
	assertNoChange(t, changes)
}

// TestSeqGapDoesNotFireEpochChange pins spec §2.4: a gap in the seq run is a
// dropped frame that the next full snapshot repairs, not evidence that the
// broker restarted. Escalating it would make an ordinary lossy frame look like a
// peer change.
func TestSeqGapDoesNotFireEpochChange(t *testing.T) {
	_, conn, changes := newEpochRecorder(t)

	writeRawNotif(t, conn, BrokerEpochMethod, "epoch-A", 1)
	if got := awaitChange(t, changes); got != "epoch-A" {
		t.Fatalf("first callback epoch = %q, want epoch-A", got)
	}

	// seq jumps 1 -> 99 with the epoch unchanged: a gap, and nothing else.
	writeRawNotif(t, conn, "discovery:nodes-changed", "epoch-A", 99)
	assertNoChange(t, changes)

	// The stream keeps running past the gap without an epoch change.
	writeRawNotif(t, conn, "heartbeat", "epoch-A", 100)
	assertNoChange(t, changes)
}

// TestLowerSeqStillAppliesEpochChange is the mirror of the gap case: seq order
// is the broker's guarantee about ITS OWN frames, not a precondition for
// trusting the epoch on a frame we did receive. An out-of-order frame still
// carries the truth about which broker is on the other end.
func TestLowerSeqStillAppliesEpochChange(t *testing.T) {
	client, conn, changes := newEpochRecorder(t)

	writeRawNotif(t, conn, BrokerEpochMethod, "epoch-A", 50)
	if got := awaitChange(t, changes); got != "epoch-A" {
		t.Fatalf("first callback epoch = %q, want epoch-A", got)
	}

	// seq 5 is far below the 50 already seen, but the epoch really did change.
	writeRawNotif(t, conn, BrokerEpochMethod, "epoch-B", 5)
	if got := awaitChange(t, changes); got != "epoch-B" {
		t.Fatalf("callback epoch = %q for the out-of-order frame, want epoch-B", got)
	}
	if got := client.epoch(); got != "epoch-B" {
		t.Fatalf("epoch() = %q after the out-of-order frame, want epoch-B", got)
	}
}

// TestEmptyEpochIsIgnored keeps an unset/blank epoch from being mistaken for a
// peer change: it is indistinguishable from "not known yet", and letting it
// overwrite a real epoch would make the next real frame look like a change.
func TestEmptyEpochIsIgnored(t *testing.T) {
	client := NewClient(new(bytes.Buffer), io.Discard)
	if client.setEpoch("") {
		t.Fatal("setEpoch(\"\") reported a change from the unknown state")
	}
	if !client.setEpoch("epoch-A") {
		t.Fatal("setEpoch on the first real epoch did not report a change")
	}
	if client.setEpoch("") {
		t.Fatal("setEpoch(\"\") overwrote a held epoch")
	}
	if got := client.epoch(); got != "epoch-A" {
		t.Fatalf("epoch() = %q after an empty epoch was offered, want epoch-A", got)
	}
}

// TestEpochNotificationStillReachesTheNotificationChannel pins that consuming
// the epoch is not a substitute for delivery: a UI reading the notification
// stream sees broker:epoch like any other frame, whether or not it registered a
// callback.
func TestEpochNotificationStillReachesTheNotificationChannel(t *testing.T) {
	client, server, _ := newPair(t)

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- server.Notify(BrokerEpochMethod, json.RawMessage(`{}`))
	}()

	// The callback-less client still records the epoch and still fans the frame
	// out; neither path may swallow the other.
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
			if got := client.epoch(); got != jsonrpc.Epoch() {
				t.Fatalf("epoch() = %q, want %q even with no callback registered", got, jsonrpc.Epoch())
			}
			return
		case <-deadline:
			t.Fatal("broker:epoch never reached the notification channel")
		}
	}
}
