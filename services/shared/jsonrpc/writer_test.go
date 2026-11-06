// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func leakCheckedPipe(t *testing.T) (*Codec, net.Conn) {
	t.Helper()
	baseline := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, baseline) })
	local, remote := net.Pipe()
	c := NewCodec(local)
	t.Cleanup(func() { _ = remote.Close(); _ = c.Close() })
	return c, remote
}

func TestNotifyStalledReaderReturnsPromptly(t *testing.T) {
	c, _ := leakCheckedPipe(t)
	done := make(chan error, 1)
	go func() { done <- c.Notify("workers:update", map[string]int{"workers": 12}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Notify blocked on a stalled reader")
	}
}

func TestResponsesStalledReaderReturnPromptly(t *testing.T) {
	c, _ := leakCheckedPipe(t)
	id := json.RawMessage("1")
	done := make(chan error, 1)
	go func() { done <- c.Respond(&id, true) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("response blocked request dispatch on a stalled reader")
	}
}

func TestWriterConcurrentResponseNotificationOrder(t *testing.T) {
	c, remote := leakCheckedPipe(t)
	const producers = 64
	var wg sync.WaitGroup
	for i := range producers {
		wg.Go(func() {
			id := json.RawMessage(strconv.Itoa(i))
			if err := c.Notify("before", i); err != nil {
				t.Error(err)
				return
			}
			if err := c.Respond(&id, i); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait() // all accepted while the remote deliberately reads nothing
	reader := NewCodec(remote)
	seen := make(map[int]bool)
	seq := uint64(0)
	for range producers * 2 {
		msg, err := reader.Read()
		if err != nil {
			t.Fatal(err)
		}
		if msg.IsNotification() {
			seq++
			if msg.Seq == nil || *msg.Seq != seq {
				t.Fatalf("sequence at %d: %v", seq, msg.Seq)
			}
			var i int
			if err := json.Unmarshal(msg.Params, &i); err != nil {
				t.Fatal(err)
			}
			seen[i] = true
		} else {
			var i int
			if err := json.Unmarshal(msg.Result, &i); err != nil {
				t.Fatal(err)
			}
			if !seen[i] || msg.Epoch != nil || msg.Seq != nil {
				t.Fatalf("response overtook notification or acquired a stamp: %+v", msg)
			}
		}
	}
	if err := c.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestWriterQueueOverflowTerminatesStream(t *testing.T) {
	c, remote := leakCheckedPipe(t)
	// The active write and queued frames together consume the admission bound.
	for range WriteQueueFrames {
		if err := c.Notify("queued", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Notify("overflow", nil); !errors.Is(err, ErrWriteQueueFull) {
		t.Fatalf("overflow: %v", err)
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("overflow left stream alive")
	}
	if err := c.Notify("after-overflow", nil); !errors.Is(err, ErrWriteQueueFull) {
		t.Fatalf("reused dead stream: %v", err)
	}
	_ = remote.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadAll(remote); err != nil {
		t.Fatalf("overflow did not close stream: %v", err)
	}
}

func TestWriterByteOverflowTerminatesStream(t *testing.T) {
	c, _ := leakCheckedPipe(t)
	if err := c.Notify("too-large", strings.Repeat("x", WriteQueueBytes)); !errors.Is(err, ErrWriteQueueFull) {
		t.Fatalf("byte bound: %v", err)
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("byte overflow left stream alive")
	}
}

func TestWriterDeadlineTerminatesStream(t *testing.T) {
	c, remote := leakCheckedPipe(t)
	start := time.Now()
	if err := c.Notify("stalled", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
	case <-time.After(WriteTimeout + time.Second):
		t.Fatal("stalled write outlived deadline")
	}
	if time.Since(start) < WriteTimeout {
		t.Fatal("deadline fired too soon")
	}
	if err := c.Notify("later", nil); !errors.Is(err, ErrWriteTimeout) {
		t.Fatalf("deadline error: %v", err)
	}
	_ = remote.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadAll(remote); err != nil {
		t.Fatalf("deadline did not close stream: %v", err)
	}
}

func TestWriterCleanCloseFlushesQueuedFrames(t *testing.T) {
	c, remote := leakCheckedPipe(t)
	const frames = 16
	for range frames {
		if err := c.Notify("queued", nil); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- c.Close() }()
	reader := NewCodec(remote)
	for i := range frames {
		msg, err := reader.Read()
		if err != nil || msg.Seq == nil || *msg.Seq != uint64(i+1) {
			t.Fatalf("flush frame %d: %+v, %v", i, msg, err)
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("clean Close did not finish after flushing")
	}
	if _, err := reader.Read(); err != io.EOF {
		t.Fatalf("Close did not close transport: %v", err)
	}
}

func TestWriterCleanCloseIsBoundedWhenReaderStalls(t *testing.T) {
	c, _ := leakCheckedPipe(t)
	if err := c.Notify("queued", nil); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Close() }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrWriteTimeout) {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(WriteTimeout + time.Second):
		t.Fatal("Close waited forever to flush")
	}
}

type observedFlushContext struct {
	context.Context
	once    sync.Once
	entered chan struct{}
}

func (c *observedFlushContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestFlushDoesNotWaitForLaterAdmissions(t *testing.T) {
	c, remote := leakCheckedPipe(t)
	if err := c.Notify("before-barrier", nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	observed := &observedFlushContext{Context: ctx, entered: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- c.Flush(observed) }()
	<-observed.entered // Flush has captured its barrier before this admission.
	if err := c.Notify("after-barrier", nil); err != nil {
		t.Fatal(err)
	}
	msg, err := NewCodec(remote).Read()
	if err != nil || msg.Method != "before-barrier" {
		t.Fatalf("first frame: %+v, %v", msg, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Flush waited for a later frame: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Flush did not finish at its submission barrier")
	}
}

type failingDiagnosticStream struct{}

func (failingDiagnosticStream) Read([]byte) (int, error) { return 0, io.EOF }
func (failingDiagnosticStream) Write([]byte) (int, error) {
	return 0, errors.New("private-transport-error")
}
func (failingDiagnosticStream) Close() error { return nil }

func TestWriterTerminalDiagnosticOnceWithoutPayloads(t *testing.T) {
	var logs syncBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	c := NewCodec(failingDiagnosticStream{})
	_ = c.Notify("private-method", map[string]string{"private-field": "private-value"})
	_ = c.Notify("later", nil)
	_ = c.Close() // join the single asynchronous diagnostic
	output := string(logs.Bytes())
	if strings.Count(output, "JSON-RPC stream terminated") != 1 {
		t.Fatalf("terminal log count: %q", output)
	}
	for _, private := range []string{"private-transport-error", "private-method", "private-field", "private-value"} {
		if strings.Contains(output, private) {
			t.Fatalf("terminal log exposed %s", private)
		}
	}
}

func TestDrainStopsAdmissionAndKeepsReadHalfOpen(t *testing.T) {
	c, remote := leakCheckedPipe(t)
	if err := c.Notify("last", nil); err != nil {
		t.Fatal(err)
	}
	observed := &observedFlushContext{Context: t.Context(), entered: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- c.Drain(observed) }()
	<-observed.entered
	if err := c.Notify("too-late", nil); !errors.Is(err, ErrCodecClosed) {
		t.Fatalf("Drain allowed new admission: %v", err)
	}
	if _, err := NewCodec(remote).Read(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	go func() {
		_, err := io.WriteString(remote, `{"jsonrpc":"2.0","id":1,"method":"still-readable"}`+"\n")
		done <- err
	}()
	msg, err := c.Read()
	if err != nil || msg.Method != "still-readable" {
		t.Fatalf("Drain closed read half: %+v, %v", msg, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWriterContextCancellationInterruptsWrite(t *testing.T) {
	c, _ := leakCheckedPipe(t)
	ctx, cancel := context.WithCancel(context.Background())
	c.BindWriteContext(ctx)
	if err := c.Notify("queued", nil); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("context cancellation did not kill stream")
	}
}
