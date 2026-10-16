// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"nvpair-shared/jsonrpc"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// notifyBuffer bounds the queue of server pushes awaiting consumption. A
// healthy TUI drains it promptly; the buffer just absorbs bursts (e.g. a
// flurry of discovery/workload events) without forcing the read loop to
// block on the UI between frames.
const notifyBuffer = 256

// BrokerEpochMethod is the notification by which the broker announces its
// protocol epoch (relay protocol v2 §2.2) without the client having to send
// broker:resync first. Every notification already carries the epoch in its
// envelope, but the broker emits this one on the first heartbeat tick after a
// connection starts, so a client sees the epoch within one interval even when
// nothing else is happening on the wire.
//
// It is a notification on purpose: validate() in shared/jsonrpc rejects an
// epoch or seq stamped onto a request or response.
const BrokerEpochMethod = "broker:epoch"

// Client multiplexes requests and server pushes over a single Codec. It
// assigns monotonic numeric ids to outbound requests, matches each
// response back to its waiting caller, and fans notifications out on a
// channel for the UI to consume. A Client is single-connection: create a
// new one per broker process.
type Client struct {
	codec  *Codec
	nextID atomic.Int64

	pendingMu sync.Mutex
	pending   map[int64]chan *Message

	notifications chan *Message

	closeOnce        sync.Once
	failureMu        sync.Mutex
	closeErr         error
	heartbeatTimeout time.Duration
	reader           io.Reader
	writer           io.Writer
	sessionContext   context.Context
	cancel           context.CancelFunc
	done             chan struct{}
}

// NewClient builds a Client over the broker's stdout (r) and stdin (w).
// Call Run to start the read loop before issuing requests.
func NewClient(r io.Reader, w io.Writer) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	client := &Client{
		sessionContext: ctx, cancel: cancel,
		codec:  NewCodec(readWriter{Reader: r, Writer: w}),
		reader: r, writer: w, heartbeatTimeout: jsonrpc.HeartbeatTimeout,
		pending:       make(map[int64]chan *Message),
		notifications: make(chan *Message, notifyBuffer),
		done:          make(chan struct{}),
	}
	client.codec.BindWriteContext(ctx)
	return client
}

// Notifications returns the channel of server-pushed frames (broker
// notifications such as app:ready, errors:update, discovery:nodes-changed).
// The channel is closed when Run returns.
func (c *Client) Notifications() <-chan *Message { return c.notifications }

// Run drives the read loop until the stream closes or ctx is cancelled.
// On exit it unblocks every pending Call and closes the notifications
// channel so consumers observe the disconnect. It is safe to call once.
func (c *Client) Run(ctx context.Context) (runErr error) {
	stopParent := context.AfterFunc(ctx, c.cancel)
	defer stopParent()
	ctx = c.sessionContext
	defer c.cancel()
	closeTransport := func() {
		// EOF on the owned control pipe asks the broker to gracefully stop its
		// workers. Keep stdout drained until the owner joins it: closing that
		// read end here could SIGPIPE the broker before worker cleanup.
		if closer, ok := c.writer.(io.Closer); ok {
			_ = closer.Close()
		}
	}
	stopClose := context.AfterFunc(ctx, closeTransport)
	defer stopClose()
	deadline := &silenceDeadline{duration: c.heartbeatTimeout, expire: func() {
		c.setError(ErrHeartbeatTimeout)
		c.cancel()
		closeTransport()
	}}
	defer func() {
		deadline.stop()
		if err := c.Err(); err != nil {
			runErr = err
		} else {
			c.setError(runErr)
		}
		c.shutdown()
	}()
	deadline.reset()
	frames := make(chan clientReadResult)
	go c.readFrames(ctx, frames)
	for {
		var frame clientReadResult
		select {
		case <-ctx.Done():
			return ctx.Err()
		case frame = <-frames:
		}
		msg, err := frame.message, frame.err
		if err != nil {
			if err == io.EOF {
				return nil
			}
			var de *jsonrpc.DecodeError
			if errors.As(err, &de) {
				continue
			}
			slog.Warn("JSON-RPC read error (terminal)", "err", err)
			return err
		}
		deadline.reset()
		switch {
		case msg.IsResponse():
			c.deliverResponse(msg)
		case msg.IsNotification():
			select {
			case c.notifications <- msg:
			case <-ctx.Done():
				return ctx.Err()
			case <-c.done:
				return c.closeErr
			}
		}
	}
}

type clientReadResult struct {
	message *Message
	err     error
}

// The pump exclusively owns Codec.Read. Once the session is invalidated it
// drains bytes without decoding/dispatch until the process owner joins the old
// broker and closes its stdout. Run and pending callers need not wait for it.
func (c *Client) readFrames(ctx context.Context, frames chan<- clientReadResult) {
	for {
		if ctx.Err() != nil {
			_, _ = io.Copy(io.Discard, c.reader)
			return
		}
		message, err := c.codec.Read()
		select {
		case frames <- clientReadResult{message, err}:
		case <-ctx.Done():
			_, _ = io.Copy(io.Discard, c.reader)
			return
		}
		if err != nil {
			var de *jsonrpc.DecodeError
			if !errors.As(err, &de) {
				// A mismatch is terminal for dispatch, not permission to break stdout
				// while the broker is still unwinding its owned worker shutdown.
				var mismatch *jsonrpc.ProtocolMismatchError
				if errors.As(err, &mismatch) {
					_, _ = io.Copy(io.Discard, c.reader)
				}
				return
			}
		}
	}
}

func (c *Client) deliverResponse(msg *Message) {
	var id int64
	if err := json.Unmarshal(*msg.ID, &id); err != nil {
		return
	}
	c.pendingMu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.pendingMu.Unlock()
	if ch != nil {
		ch <- msg
	}
}

// Err is the terminal session reason, published before Notifications closes.
func (c *Client) Err() error { c.failureMu.Lock(); defer c.failureMu.Unlock(); return c.closeErr }
func (c *Client) setError(err error) {
	c.failureMu.Lock()
	defer c.failureMu.Unlock()
	if c.closeErr == nil {
		c.closeErr = err
	}
}

func (c *Client) shutdown() {
	c.closeOnce.Do(func() {
		close(c.done)
		c.pendingMu.Lock()
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.pendingMu.Unlock()
		close(c.notifications)
	})
}

// Call sends a request and blocks until the matching response arrives,
// ctx is cancelled, or the connection closes. A JSON-RPC error response
// is returned as a non-nil error (*RPCError).
func (c *Client) Call(ctx context.Context, method string, params any) (*Message, error) {
	id := c.nextID.Add(1)
	ch := make(chan *Message, 1)

	c.pendingMu.Lock()
	select {
	case <-c.done:
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("connection closed before %q", method)
	default:
	}
	c.pending[id] = ch
	c.pendingMu.Unlock()

	if err := c.writeRequest(id, method, params); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, err
	}

	select {
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, ctx.Err()
	case <-c.done:
		return nil, fmt.Errorf("connection closed before response to %q", method)
	case msg, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("connection closed before response to %q", method)
		}
		if msg.Error != nil {
			return msg, msg.Error
		}
		return msg, nil
	}
}

// writeRequest sends an id-bearing request. It carries no envelope stamp:
// jsonrpc.validate() rejects epoch/seq on anything that is not a notification,
// so stamping a request would make the frame unreadable to a v2 peer.
func (c *Client) writeRequest(id int64, method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	rawID := json.RawMessage(strconv.FormatInt(id, 10))
	return c.codec.Request(&rawID, method, raw)
}

// Notify sends a fire-and-forget notification (no id, no response). The
// canonical codec stamps it with this connection's epoch and next seq inside
// its own write lock, so the wire reads in seq order even when several
// goroutines notify at once. Monotonicity is a sender guarantee; the receiver
// accepts gaps and regressions without resync.
func (c *Client) Notify(method string, params any) error {
	return c.codec.Notify(method, params)
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	return json.Marshal(params)
}
