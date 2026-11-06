// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"
)

// Limits include the in-flight frame. See README.md for burst measurements.
const (
	WriteQueueFrames = 256
	WriteQueueBytes  = 16 << 20
	WriteTimeout     = 5 * time.Second
	// Wait briefly for the common immediately-written case, never for a pipe.
	WriteAdmissionWait = 10 * time.Millisecond
)

var (
	ErrWriteQueueFull = errors.New("jsonrpc write queue full")
	ErrWriteTimeout   = errors.New("jsonrpc write deadline exceeded")
	ErrCodecClosed    = errors.New("jsonrpc codec closed")
)

type queuedFrame struct {
	data     []byte
	deadline time.Time
	done     chan struct{}
	ctx      context.Context
}

type orderedWriter struct {
	mu          sync.Mutex
	queue       []queuedFrame
	bytes       int
	frames      int
	running     bool
	closing     bool
	err         error
	done        chan struct{}
	idle        chan struct{}
	tail        <-chan struct{}
	cleanupDone chan struct{}
	closeOnce   sync.Once
	stopContext func() bool
}

func (c *Codec) initWriter() {
	c.out.done = make(chan struct{})
	c.out.idle = make(chan struct{})
	close(c.out.idle)
	c.out.tail = c.out.idle
	c.out.cleanupDone = make(chan struct{})
}

// BindWriteContext terminates the stream on cancellation. Install before use.
// Cancellation is abortive; for a clean shutdown call Close before canceling.
func (c *Codec) BindWriteContext(ctx context.Context) {
	c.out.stopContext = context.AfterFunc(ctx, func() { c.terminate(ctx.Err()) })
}

// Done closes on terminal write failure or Close, including queue overflow.
func (c *Codec) Done() <-chan struct{} { return c.out.done }

// Err reports the terminal stream reason, or nil while it is live.
func (c *Codec) Err() error { c.out.mu.Lock(); defer c.out.mu.Unlock(); return c.out.err }

func (c *Codec) write(msg *Message) error             { return c.enqueue(context.Background(), msg) }
func (c *Codec) writeNotification(msg *Message) error { return c.write(msg) }

func (c *Codec) submit(ctx context.Context, msg *Message) (<-chan struct{}, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.out.mu.Lock()
	if c.out.err != nil || c.out.closing {
		err := c.out.err
		if err == nil {
			err = ErrCodecClosed
		}
		c.out.mu.Unlock()
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		c.out.mu.Unlock()
		return nil, err
	}
	if msg.IsNotification() {
		c.stamp(msg)
	}
	data, err := json.Marshal(msg)
	if err != nil {
		c.out.mu.Unlock()
		return nil, err
	}
	data = append(data, '\n')
	if c.out.frames >= WriteQueueFrames || len(data) > WriteQueueBytes-c.out.bytes {
		// Set terminal state before releasing the enqueue lock: no later frame
		// may be accepted after the rejected notification consumed a sequence.
		c.failLocked(ErrWriteQueueFull)
		c.out.mu.Unlock()
		go c.finishTermination(ErrWriteQueueFull)
		return nil, ErrWriteQueueFull
	}
	if !c.out.running {
		c.out.running = true
		c.out.idle = make(chan struct{})
	}
	start := c.out.frames == 0
	frame := queuedFrame{data: data, deadline: time.Now().Add(WriteTimeout), done: make(chan struct{}), ctx: ctx}
	c.out.queue = append(c.out.queue, frame)
	c.out.tail = frame.done
	c.out.frames++
	c.out.bytes += len(data)
	c.out.mu.Unlock()
	if start {
		go c.drain()
	}
	return frame.done, nil
}

func (c *Codec) enqueue(ctx context.Context, msg *Message) error {
	done, err := c.submit(ctx, msg)
	if err != nil {
		return err
	}
	// Acceptance is not delivery. The brief completion fast path avoids a
	// scheduler round trip for shutdown acknowledgements, but a stalled reader
	// never keeps a producer (including a lock holder) here beyond this wait.
	wait := WriteAdmissionWait
	if msg.IsRequest() {
		wait = WriteTimeout
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
		if err := ctx.Err(); err != nil {
			return err
		}
		c.out.mu.Lock()
		err := c.out.err
		c.out.mu.Unlock()
		return err
	case <-c.out.done:
		c.out.mu.Lock()
		err := c.out.err
		c.out.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		if msg.IsRequest() {
			c.terminate(ErrWriteTimeout)
			return ErrWriteTimeout
		}
		return nil
	}
}

// failLocked never logs an underlying error: a custom transport's error can
// contain message contents. Only these fixed transport categories reach logs.
func (c *Codec) failLocked(err error) {
	c.out.err = err
	c.out.queue = nil
	close(c.out.done)
}

func (c *Codec) terminate(err error) {
	c.out.mu.Lock()
	first := c.out.err == nil
	if first {
		c.failLocked(err)
	}
	c.out.mu.Unlock()
	if first {
		// Neither transport Close nor a stderr logger may run on a producer's
		// lock-holding path. Only one terminal cleanup goroutine is ever started.
		go c.finishTermination(err)
	}
}

func (c *Codec) finishTermination(err error) {
	defer close(c.out.cleanupDone)
	c.closeStream()
	reason := "write failed"
	if errors.Is(err, ErrWriteQueueFull) {
		reason = "queue full"
	}
	if errors.Is(err, ErrWriteTimeout) {
		reason = "write deadline"
	}
	if !errors.Is(err, ErrCodecClosed) && !errors.Is(err, ErrPeerClosed) && !errors.Is(err, context.Canceled) {
		slog.Warn("JSON-RPC stream terminated", "reason", reason)
	}
}

func (c *Codec) closeStream() {
	c.out.closeOnce.Do(func() {
		if closer, ok := c.writer.(io.Closer); ok {
			_ = closer.Close()
		}
	})
}

func (c *Codec) drain() {
	for {
		c.out.mu.Lock()
		if c.out.err != nil || len(c.out.queue) == 0 {
			c.out.running = false
			close(c.out.idle)
			c.out.mu.Unlock()
			return
		}
		frame := c.out.queue[0]
		c.out.queue[0] = queuedFrame{}
		c.out.queue = c.out.queue[1:]
		c.out.mu.Unlock()
		timer := time.AfterFunc(time.Until(frame.deadline), func() { c.terminate(ErrWriteTimeout) })
		var err error
		if frame.ctx.Err() == nil {
			err = writeFrame(c.writer, frame.data)
		}
		timer.Stop()
		if err != nil {
			c.terminate(err)
			c.out.mu.Lock()
			c.out.running = false
			close(c.out.idle)
			c.out.mu.Unlock()
			return
		}
		c.out.mu.Lock()
		c.out.bytes -= len(frame.data)
		c.out.frames--
		close(frame.done)
		// Transfer idle ownership under the enqueue lock, so a producer cannot
		// append to an empty queue while this goroutine is exiting.
		if c.out.frames == 0 {
			c.out.running = false
			close(c.out.idle)
			c.out.mu.Unlock()
			return
		}
		c.out.mu.Unlock()
	}
}

func writeFrame(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

// Flush waits for frames accepted before this call, never longer than
// WriteTimeout (or ctx). A timeout kills the stream, not just the waiter.
func (c *Codec) Flush(ctx context.Context) error {
	c.out.mu.Lock()
	if c.out.err != nil {
		err := c.out.err
		c.out.mu.Unlock()
		return err
	}
	done := c.out.tail
	c.out.mu.Unlock()
	timer := time.NewTimer(WriteTimeout)
	defer timer.Stop()
	select {
	case <-c.out.done:
	case <-done:
		c.out.mu.Lock()
		err := c.out.err
		c.out.mu.Unlock()
		return err
	case <-ctx.Done():
		c.terminate(ctx.Err())
	case <-timer.C:
		c.terminate(ErrWriteTimeout)
	}
	c.out.mu.Lock()
	err := c.out.err
	c.out.mu.Unlock()
	return err
}

// Drain stops admission and flushes accepted frames, leaving the read half
// open for a process owner's cleanup join. The owner must then close control.
func (c *Codec) Drain(ctx context.Context) error {
	c.out.mu.Lock()
	c.out.closing = true
	c.out.mu.Unlock()
	return c.Flush(ctx)
}

// Close stops admission, flushes accepted frames within one total WriteTimeout,
// then closes the transport. Transports must implement Close that interrupts
// Read/Write. Non-closeable in-memory codecs are useful only for finite writes.
func (c *Codec) Close() error {
	deadline := time.Now().Add(WriteTimeout)
	err := c.Drain(context.Background())
	c.terminate(ErrCodecClosed)
	c.closeStream()
	// Join terminal logging too, without adding a second shutdown allowance.
	if remaining := time.Until(deadline); remaining > 0 {
		timer := time.NewTimer(remaining)
		select {
		case <-c.out.cleanupDone:
		case <-timer.C:
		}
		timer.Stop()
	}
	if c.out.stopContext != nil {
		c.out.stopContext()
	}
	return err
}
