// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"nvpair-shared/jsonrpc"
)

// newResyncPipe returns a broker side wired to a client-side frame reader. The
// test reads frames on the returned *bufio.Scanner, so every frame the broker
// writes is drained by the test rather than blocking net.Pipe.
func newResyncPipe(t *testing.T) (*bufio.Scanner, net.Conn, net.Conn) {
	t.Helper()
	brokerSide, clientSide := net.Pipe()
	t.Cleanup(func() {
		_ = brokerSide.Close()
		_ = clientSide.Close()
	})
	return bufio.NewScanner(clientSide), brokerSide, clientSide
}

func newFakeScannerPath(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("serve wiring test uses a POSIX shell helper")
	}
	path := t.TempDir() + "/fake-scanner.sh"
	if err := os.WriteFile(path, []byte("#!/bin/sh\ncat >/dev/null\n"), 0o700); err != nil {
		t.Fatalf("write fake scanner: %v", err)
	}
	return path
}

func newServeHarness(t *testing.T, heartbeatInterval time.Duration) (*Codec, net.Conn, context.CancelFunc, <-chan error) {
	t.Helper()
	brokerSide, clientSide := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	b := NewBroker(NewCodec(brokerSide), workerPaths{scanner: newFakeScannerPath(t)})
	b.heartbeatInterval = heartbeatInterval
	b.closeTransport = brokerSide.Close
	done := make(chan error, 1)
	go func() {
		done <- b.Serve(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		_ = brokerSide.Close()
		_ = clientSide.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Serve did not return during cleanup")
		}
	})
	return NewCodec(clientSide), clientSide, cancel, done
}

func readServeMessage(t *testing.T, codec *Codec, conn net.Conn, timeout time.Duration) *Message {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	msg, err := codec.Read()
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear read deadline: %v", err)
	}
	if err != nil {
		t.Fatalf("read serve frame: %v", err)
	}
	return msg
}

func readUntilMethod(t *testing.T, codec *Codec, conn net.Conn, method string) *Message {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msg := readServeMessage(t, codec, conn, time.Until(deadline))
		if msg.Method == method {
			return msg
		}
	}
	t.Fatalf("method %s did not arrive", method)
	return nil
}

// readResyncFrame reads one wire frame as raw JSON object keys plus decoded
// values. It returns the top-level keys so a test can assert on the presence
// or absence of the envelope fields without printing frame bytes.
func readResyncFrame(t *testing.T, sc *bufio.Scanner, conn net.Conn, timeout time.Duration) (map[string]json.RawMessage, map[string]any) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if !sc.Scan() {
		t.Fatalf("scan frame: %v", sc.Err())
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear read deadline: %v", err)
	}
	rawKeys := map[string]json.RawMessage{}
	if err := json.Unmarshal(sc.Bytes(), &rawKeys); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	decoded := map[string]any{}
	if err := json.Unmarshal(sc.Bytes(), &decoded); err != nil {
		t.Fatalf("decode frame values: %v", err)
	}
	return rawKeys, decoded
}

type blockingReadWriter struct {
	writeStarted chan struct{}
	unblock      chan struct{}
	once         sync.Once
}

func newBlockingReadWriter() *blockingReadWriter {
	return &blockingReadWriter{
		writeStarted: make(chan struct{}),
		unblock:      make(chan struct{}),
	}
}

func (w *blockingReadWriter) Read(p []byte) (int, error) {
	return 0, io.EOF
}

func (w *blockingReadWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.writeStarted) })
	<-w.unblock
	return 0, io.ErrClosedPipe
}

func (w *blockingReadWriter) Close() error {
	select {
	case <-w.unblock:
	default:
		close(w.unblock)
	}
	return nil
}

type noopCloseBlockingReadWriter struct {
	writeStarted chan struct{}
	once         sync.Once
}

func newNoopCloseBlockingReadWriter() *noopCloseBlockingReadWriter {
	return &noopCloseBlockingReadWriter{writeStarted: make(chan struct{})}
}

func (w *noopCloseBlockingReadWriter) Read(p []byte) (int, error) {
	return 0, io.EOF
}

func (w *noopCloseBlockingReadWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.writeStarted) })
	select {}
}

func (w *noopCloseBlockingReadWriter) Close() error {
	return nil
}

// TestHeartbeatStopBoundsNoopTransportClose covers stdio-shaped shutdown: Close
// is a no-op, so a write blocked below the codec cannot be unblocked. stop must
// abandon the join rather than park Serve forever, and it must log that single
// abandonment without including any payload.
func TestHeartbeatStopBoundsNoopTransportClose(t *testing.T) {
	writer := newNoopCloseBlockingReadWriter()
	b := &Broker{
		codec:             NewCodec(writer),
		heartbeatInterval: 5 * time.Millisecond,
		closeTransport:    writer.Close,
	}

	var logs lockedBuffer
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldOutput) })

	ctx, cancel := context.WithCancel(context.Background())
	stopHeartbeat := b.startHeartbeat(ctx, cancel)

	select {
	case <-writer.writeStarted:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("heartbeat never entered the blocking writer")
	}

	stopped := make(chan struct{})
	started := time.Now()
	go func() {
		defer close(stopped)
		stopHeartbeat()
	}()

	select {
	case <-stopped:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("stopHeartbeat did not return after bounded wait on a no-op close transport")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("stopHeartbeat returned after %v, want within the bounded wait", elapsed)
	}
	got := string(logs.Bytes())
	if !strings.Contains(got, "heartbeat") || !strings.Contains(got, "250ms") {
		t.Fatalf("heartbeat shutdown warning = %q, want method and timeout", got)
	}
}

// TestHeartbeatStopJoinsWhenTransportCloseUnblocks proves the clean shutdown
// path still waits for the heartbeat goroutine to leave the codec write once the
// transport close unblocks it.
func TestHeartbeatStopJoinsWhenTransportCloseUnblocks(t *testing.T) {
	writer := newBlockingReadWriter()
	b := &Broker{
		codec:             NewCodec(writer),
		heartbeatInterval: 5 * time.Millisecond,
		closeTransport:    writer.Close,
	}

	var logs lockedBuffer
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldOutput) })

	ctx, cancel := context.WithCancel(context.Background())
	stopHeartbeat := b.startHeartbeat(ctx, cancel)

	select {
	case <-writer.writeStarted:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("heartbeat never entered the blocking writer")
	}

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stopHeartbeat()
	}()

	select {
	case <-stopped:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("stopHeartbeat did not join after transport close unblocked the writer")
	}
	got := string(logs.Bytes())
	if strings.Contains(got, "abandon") {
		t.Fatalf("clean heartbeat stop logged abandonment: %q", got)
	}
}

// TestHeartbeatCancelClosesBlockedWriteAndReturns proves cancellation while the
// heartbeat goroutine is already inside a blocked codec write does not leave it
// holding the codec write mutex forever.
func TestHeartbeatCancelClosesBlockedWriteAndReturns(t *testing.T) {
	writer := newBlockingReadWriter()
	b := &Broker{
		codec:             NewCodec(writer),
		heartbeatInterval: 5 * time.Millisecond,
		closeTransport:    writer.Close,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.runHeartbeat(ctx)
	}()

	select {
	case <-writer.writeStarted:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("heartbeat never entered the blocking writer")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runHeartbeat did not return after cancellation closed a blocked write")
	}
}

func TestServeDispatchesBrokerResyncRequest(t *testing.T) {
	codec, conn, cancel, done := newServeHarness(t, jsonrpc.HeartbeatInterval)
	readUntilMethod(t, codec, conn, "app:ready")

	id := json.RawMessage(`101`)
	if err := codec.Request(&id, "broker:resync", nil); err != nil {
		t.Fatalf("send broker:resync request: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msg := readServeMessage(t, codec, conn, time.Until(deadline))
		if !msg.IsResponse() || msg.ID == nil || strings.TrimSpace(string(*msg.ID)) != "101" {
			continue
		}
		var result ResyncResult
		if err := json.Unmarshal(msg.Result, &result); err != nil {
			t.Fatalf("decode broker:resync result: %v", err)
		}
		if result.Epoch != jsonrpc.Epoch() {
			t.Fatalf("resync result epoch = %q, want %q", result.Epoch, jsonrpc.Epoch())
		}
		if result.Protocol != jsonrpc.ProtocolVersion {
			t.Fatalf("resync result protocol = %d, want %d", result.Protocol, jsonrpc.ProtocolVersion)
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Serve returned error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Serve did not return after cancel")
		}
		return
	}
	t.Fatal("broker:resync response did not arrive through Serve")
}

func TestServeStartsHeartbeatStream(t *testing.T) {
	codec, conn, cancel, done := newServeHarness(t, 5*time.Millisecond)
	msg := readUntilMethod(t, codec, conn, "heartbeat")
	if !msg.IsNotification() {
		t.Fatalf("heartbeat arrived as %s, want notification", frameKind(msg))
	}
	if msg.Epoch == nil || msg.Seq == nil {
		t.Fatalf("heartbeat is unstamped (epoch=%v seq=%v)", msg.Epoch, msg.Seq)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}

// TestResyncResponseCarriesBrokerEpochWithoutEnvelopeStamp pins the one rule
// that makes broker:resync work on protocol v2. broker:resync is a REQUEST, so
// its reply travels the response path, which validate() holds to carrying no
// epoch and no seq — a stamped reply would be rejected as a
// *ProtocolMismatchError and read as a terminal mismatch. The broker's epoch
// reaches the client inside the result object instead.
func TestResyncResponseCarriesBrokerEpochWithoutEnvelopeStamp(t *testing.T) {
	sc, brokerSide, conn := newResyncPipe(t)
	b := &Broker{codec: NewCodec(brokerSide), heartbeatInterval: jsonrpc.HeartbeatInterval}

	id := json.RawMessage(`1`)
	respondErr := make(chan error, 1)
	go func() { respondErr <- b.handleResync(&id) }()

	keys, values := readResyncFrame(t, sc, conn, 2*time.Second)
	if err := <-respondErr; err != nil {
		t.Fatalf("handleResync: %v", err)
	}

	for _, banned := range []string{"epoch", "seq"} {
		if _, ok := keys[banned]; ok {
			t.Fatalf("broker:resync response carries envelope field %q; keys present: %v", banned, keyNames(keys))
		}
	}
	if values["jsonrpc"] != "2.0" {
		t.Fatalf("response jsonrpc = %v, want 2.0", values["jsonrpc"])
	}
	if got, ok := values["id"]; !ok || got != float64(1) {
		t.Fatalf("response id = %v (present=%v), want 1", got, ok)
	}

	rawResult, ok := keys["result"]
	if !ok {
		t.Fatalf("broker:resync response has no result; keys present: %v", keyNames(keys))
	}
	var result ResyncResult
	if err := json.Unmarshal(rawResult, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.Epoch == "" {
		t.Fatal("resync result epoch is empty; it must never reach the wire empty")
	}
	if result.Epoch != jsonrpc.Epoch() {
		t.Fatalf("resync result epoch = %q, want the broker process epoch %q", result.Epoch, jsonrpc.Epoch())
	}
	if result.Protocol != jsonrpc.ProtocolVersion {
		t.Fatalf("resync result protocol = %d, want %d", result.Protocol, jsonrpc.ProtocolVersion)
	}
}

// TestHeartbeatEmitsStampedNotificationsUntilContextCancel proves liveness and
// stopping behaviourally rather than by reading a log: a short injected
// interval must put real "heartbeat" notifications on the wire, each carrying
// this connection's epoch and a strictly increasing seq, and cancelling ctx
// must return the goroutine and leave the connection silent.
func TestHeartbeatEmitsStampedNotificationsUntilContextCancel(t *testing.T) {
	sc, brokerSide, conn := newResyncPipe(t)
	b := &Broker{codec: NewCodec(brokerSide), heartbeatInterval: 5 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.runHeartbeat(ctx)
	}()

	var lastSeq uint64
	heartbeats := 0
	for heartbeats < 3 {
		keys, values := readResyncFrame(t, sc, conn, 2*time.Second)
		method, _ := values["method"].(string)

		// Every frame on this stream is stamped and shares ONE seq run, so the
		// strictly-increasing check spans broker:epoch as well as heartbeat:
		// the epoch frame is written on the same path and must not consume a
		// separate run or land out of order.
		rawSeq, ok := keys["seq"]
		if !ok {
			t.Fatalf("%s carries no seq; keys present: %v", method, keyNames(keys))
		}
		var seq uint64
		if err := json.Unmarshal(rawSeq, &seq); err != nil {
			t.Fatalf("decode %s seq: %v", method, err)
		}
		if seq <= lastSeq {
			t.Fatalf("%s seq = %v, want strictly greater than %v", method, seq, lastSeq)
		}
		lastSeq = seq

		if method != "heartbeat" {
			// The only other frame allowed here is the one-time epoch
			// announcement; its own behaviour is pinned by the tests below, so
			// this loop only cares about the heartbeat it is collecting.
			continue
		}
		heartbeats++

		// A notification frame is the only frame validate() accepts a stamp on.
		epoch, ok := keys["epoch"]
		if !ok {
			t.Fatalf("heartbeat %d carries no epoch; keys present: %v", heartbeats, keyNames(keys))
		}
		var epochVal string
		if err := json.Unmarshal(epoch, &epochVal); err != nil {
			t.Fatalf("decode heartbeat epoch: %v", err)
		}
		if epochVal == "" {
			t.Fatalf("heartbeat %d carries an empty epoch", heartbeats)
		}
		if epochVal != jsonrpc.Epoch() {
			t.Fatalf("heartbeat %d epoch = %q, want the broker process epoch %q", heartbeats, epochVal, jsonrpc.Epoch())
		}
		if _, hasID := keys["id"]; hasID {
			t.Fatalf("heartbeat %d is not a notification: it carries an id", heartbeats)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runHeartbeat did not return within 2s of context cancel")
	}

	// Silence after stop is part of the contract: a heartbeat that kept ticking
	// past cancel would show up here.
	if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if sc.Scan() {
		t.Fatalf("runHeartbeat wrote a frame after ctx cancel: keys present: %v", keyNames(decodeFrameKeys(t, sc.Bytes())))
	} else if err := sc.Err(); err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
		// Silence past the deadline is the expected result; io.EOF and the
		// deadline timeout both mean "nothing more arrived".
		t.Fatalf("unexpected read error after ctx cancel: %v", err)
	}
}

// TestBrokerEpochIsAnnouncedAsStampedNotification pins the frame that makes the
// epoch observable to a client that never sent broker:resync. It is proven off
// the wire through a channel of decoded frames, not by grepping a log, and
// through the same shared codec a client reads with, so "the codec would have
// rejected this" cannot pass unnoticed.
func TestBrokerEpochIsAnnouncedAsStampedNotification(t *testing.T) {
	_, brokerSide, conn := newResyncPipe(t)
	b := &Broker{codec: NewCodec(brokerSide), heartbeatInterval: 5 * time.Millisecond}

	frames := make(chan *jsonrpc.Message, 64)
	go func() {
		// A client-side Codec over the same pipe: exactly what nvpair-tui and
		// the desktop bridge do with the broker's stdout.
		codec := NewCodec(conn)
		for {
			msg, err := codec.Read()
			if err != nil {
				close(frames)
				return
			}
			frames <- msg
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.runHeartbeat(ctx)
	}()

	// Wait for the announcement rather than sleeping a fixed interval, so the
	// test is not racing the ticker.
	var epochMsg *jsonrpc.Message
	deadline := time.After(5 * time.Second)
	for epochMsg == nil {
		select {
		case msg, ok := <-frames:
			if !ok {
				cancel()
				<-done
				t.Fatal("client connection closed before broker:epoch arrived")
			}
			if msg.Method == brokerEpochMethod {
				epochMsg = msg
			}
		case <-deadline:
			cancel()
			<-done
			t.Fatal("broker:epoch was never announced on the client stream")
		}
	}
	cancel()
	<-done

	// It reached a codec.Read() at all, so validate() already accepted it.
	// Being a notification is what makes that possible, and it must stay one.
	if !epochMsg.IsNotification() {
		t.Fatalf("broker:epoch arrived as %q, want a notification", frameKind(epochMsg))
	}
	if epochMsg.ID != nil {
		t.Fatal("broker:epoch carries an id; a notification must not answer a request")
	}
	if epochMsg.Epoch == nil || epochMsg.Seq == nil {
		t.Fatalf("broker:epoch is unstamped (epoch=%v seq=%v); a notification must carry both", epochMsg.Epoch, epochMsg.Seq)
	}
	if *epochMsg.Epoch != jsonrpc.Epoch() {
		t.Fatalf("broker:epoch envelope epoch = %q, want the broker process epoch %q", *epochMsg.Epoch, jsonrpc.Epoch())
	}

	var result ResyncResult
	if err := json.Unmarshal(epochMsg.Params, &result); err != nil {
		t.Fatalf("decode broker:epoch params: %v", err)
	}
	if result.Epoch == "" {
		t.Fatal("broker:epoch params carry an empty epoch")
	}
	if result.Epoch != jsonrpc.Epoch() {
		t.Fatalf("broker:epoch params epoch = %q, want the broker process epoch %q", result.Epoch, jsonrpc.Epoch())
	}
	if result.Protocol != 2 {
		t.Fatalf("broker:epoch params protocol = %d, want 2", result.Protocol)
	}
}

// TestBrokerEpochIsAnnouncedOncePerEpochNotPerTick is the other half of the
// contract: the announcement is an edge, not a level. jsonrpc.Epoch() is stable
// for the life of the process, so many ticks must produce exactly ONE
// broker:epoch frame — otherwise the client callback this exists to drive would
// fire on every heartbeat.
func TestBrokerEpochIsAnnouncedOncePerEpochNotPerTick(t *testing.T) {
	sc, brokerSide, conn := newResyncPipe(t)
	b := &Broker{codec: NewCodec(brokerSide), heartbeatInterval: 5 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.runHeartbeat(ctx)
	}()

	// Collect raw frames so the count is read off the wire.
	const wantHeartbeats = 6
	epochFrames, heartbeatFrames := 0, 0
	deadline := time.After(5 * time.Second)
	for heartbeatFrames < wantHeartbeats {
		_, values := readResyncFrame(t, sc, conn, 5*time.Second)
		switch values["method"] {
		case brokerEpochMethod:
			epochFrames++
		case "heartbeat":
			heartbeatFrames++
		default:
			t.Fatalf("unexpected frame method %v on the heartbeat stream", values["method"])
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("only %d heartbeats arrived within the deadline", heartbeatFrames)
		default:
		}
	}
	cancel()
	<-done

	if heartbeatFrames != wantHeartbeats {
		t.Fatalf("collected %d heartbeats, want %d", heartbeatFrames, wantHeartbeats)
	}
	if epochFrames != 1 {
		t.Fatalf("broker:epoch was emitted %d times across %d ticks, want exactly 1", epochFrames, heartbeatFrames)
	}
}

// frameKind names a frame's JSON-RPC role for failure messages, without echoing
// its contents.
func frameKind(msg *jsonrpc.Message) string {
	switch {
	case msg.IsResponse():
		return "response"
	case msg.IsRequest():
		return "request"
	case msg.IsNotification():
		return "notification"
	default:
		return "unknown"
	}
}

// TestCodecAcceptsLowerAndNonconsecutiveNotificationSeq pins that seq gaps or
// decreases are not protocol mismatches at the transport layer. Consumers
// reconcile from snapshots; the codec only validates that notifications carry
// an envelope.
func TestCodecAcceptsLowerAndNonconsecutiveNotificationSeq(t *testing.T) {
	epoch := jsonrpc.Epoch()
	frames := `{"jsonrpc":"2.0","method":"alpha","params":{},"epoch":` + strconv.Quote(epoch) + `,"seq":5}` + "\n" +
		`{"jsonrpc":"2.0","method":"beta","params":{},"epoch":` + strconv.Quote(epoch) + `,"seq":3}` + "\n" +
		`{"jsonrpc":"2.0","method":"gamma","params":{},"epoch":` + strconv.Quote(epoch) + `,"seq":10}` + "\n"
	codec := NewCodec(readWriter{Reader: strings.NewReader(frames), Writer: io.Discard})

	for _, want := range []string{"alpha", "beta", "gamma"} {
		msg, err := codec.Read()
		if err != nil {
			var pm *jsonrpc.ProtocolMismatchError
			if errors.As(err, &pm) {
				t.Fatalf("%s was treated as a protocol mismatch: %v", want, err)
			}
			t.Fatalf("read %s: %v", want, err)
		}
		if msg.Method != want {
			t.Fatalf("method = %q, want %q", msg.Method, want)
		}
	}
}

// TestStampedResponseIsRejectedAndStampedNotificationIsAccepted is the direct
// proof of the envelope rule this feature sits on, run through the same
// validate() the client's read loop uses. It is what makes the resync test
// above meaningful: if a stamped reply were legal, handleResync could stamp
// and nothing would notice.
func TestStampedResponseIsRejectedAndStampedNotificationIsAccepted(t *testing.T) {
	cases := []struct {
		name       string
		frame      string
		wantReject bool
	}{
		{
			name:       "stamped response is rejected",
			frame:      `{"jsonrpc":"2.0","id":1,"result":{"epoch":"e","protocol":2},"epoch":"e","seq":1}`,
			wantReject: true,
		},
		{
			name:       "stamped error response is rejected",
			frame:      `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"x"},"epoch":"e","seq":1}`,
			wantReject: true,
		},
		{
			name:       "unstamped response is accepted",
			frame:      `{"jsonrpc":"2.0","id":1,"result":{"epoch":"e","protocol":2}}`,
			wantReject: false,
		},
		{
			name:       "stamped notification is accepted",
			frame:      `{"jsonrpc":"2.0","method":"heartbeat","params":{},"epoch":"e","seq":1}`,
			wantReject: false,
		},
		{
			name:       "stamped broker:epoch notification is accepted",
			frame:      `{"jsonrpc":"2.0","method":"broker:epoch","params":{"epoch":"e","protocol":2},"epoch":"e","seq":1}`,
			wantReject: false,
		},
		{
			name:       "unstamped notification is rejected",
			frame:      `{"jsonrpc":"2.0","method":"heartbeat","params":{}}`,
			wantReject: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader, writer := net.Pipe()
			t.Cleanup(func() {
				_ = reader.Close()
				_ = writer.Close()
			})
			go func() {
				_, _ = io.WriteString(writer, tc.frame+"\n")
			}()
			codec := NewCodec(reader)
			_ = reader.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, err := codec.Read()
			if tc.wantReject {
				var pm *jsonrpc.ProtocolMismatchError
				if !errors.As(err, &pm) {
					t.Fatalf("Read error = %v (%T), want *ProtocolMismatchError", err, err)
				}
				// Terminal by design: it must not be mistaken for a
				// recoverable *DecodeError.
				var de *jsonrpc.DecodeError
				if errors.As(err, &de) {
					t.Fatal("mismatch error must not be a *DecodeError")
				}
				return
			}
			if err != nil {
				t.Fatalf("Read error = %v, want the frame accepted", err)
			}
		})
	}
}

// keyNames returns the sorted key set of a decoded frame, for failure messages
// that name the offending field without echoing frame bytes.
func keyNames(keys map[string]json.RawMessage) []string {
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// decodeFrameKeys decodes a raw frame's top-level keys.
func decodeFrameKeys(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	keys := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	return keys
}
