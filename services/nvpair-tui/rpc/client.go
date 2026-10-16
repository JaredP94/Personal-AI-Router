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

	// epochMu guards the two fields below. They are read on the read loop and
	// written by whoever registers a callback or calls setEpoch, so they are
	// not confined to one goroutine.
	epochMu       sync.Mutex
	currentEpoch  string
	onEpochChange func(epoch string)

	closeOnce sync.Once
	closeErr  error
	done      chan struct{}
}

// NewClient builds a Client over the broker's stdout (r) and stdin (w).
// Call Run to start the read loop before issuing requests.
func NewClient(r io.Reader, w io.Writer) *Client {
	return &Client{
		codec:         NewCodec(readWriter{Reader: r, Writer: w}),
		pending:       make(map[int64]chan *Message),
		notifications: make(chan *Message, notifyBuffer),
		done:          make(chan struct{}),
	}
}

// Notifications returns the channel of server-pushed frames (broker
// notifications such as app:ready, errors:update, discovery:nodes-changed).
// The channel is closed when Run returns.
func (c *Client) Notifications() <-chan *Message { return c.notifications }

// Run drives the read loop until the stream closes or ctx is cancelled.
// On exit it unblocks every pending Call and closes the notifications
// channel so consumers observe the disconnect. It is safe to call once.
func (c *Client) Run(ctx context.Context) error {
	defer c.shutdown()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msg, err := c.codec.Read()
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
		switch {
		case msg.IsResponse():
			c.deliverResponse(msg)
		case msg.IsNotification():
			// Ahead of the fan-out: a frame that announces a new epoch is
			// still delivered to the notification channel, so a UI watching
			// the stream does not have to know this callback exists.
			c.observeEpoch(msg)
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

// OnEpochChange registers cb to be run whenever the broker's protocol epoch
// changes under this connection — a signal that the peer restarted and every
// fact this UI cached from it is suspect. Registering replaces any previous
// callback; passing nil clears it.
//
// There is no polling and no ticker behind this: the read loop already sees
// every frame, and the epoch rides on all of them, so the change is noticed on
// the frame that carries it rather than on a timer that has to guess when to
// look.
func (c *Client) OnEpochChange(cb func(epoch string)) {
	c.epochMu.Lock()
	defer c.epochMu.Unlock()
	c.onEpochChange = cb
}

// epoch returns the last protocol epoch seen on this connection, or "" before
// any notification has arrived.
func (c *Client) epoch() string {
	c.epochMu.Lock()
	defer c.epochMu.Unlock()
	return c.currentEpoch
}

// setEpoch records epoch as current and reports whether it CHANGED. An empty
// epoch is ignored: it is indistinguishable from "not known yet", and letting
// it overwrite a real epoch would make the next real frame look like a change.
func (c *Client) setEpoch(epoch string) bool {
	c.epochMu.Lock()
	defer c.epochMu.Unlock()
	if epoch == "" || epoch == c.currentEpoch {
		return false
	}
	c.currentEpoch = epoch
	return true
}

// observeEpoch applies a notification's envelope epoch and fires the change
// callback when it differs from the one held.
//
// Only the epoch is consulted. The frame's seq is deliberately NOT part of the
// decision: per spec §2.4 a seq gap is repaired by the next full snapshot, so a
// gap must not be escalated into an epoch change, and an out-of-order (lower)
// seq still carries the truth about which broker we are talking to. Gating on
// seq would mean a single dropped frame suppressed a genuine restart signal.
func (c *Client) observeEpoch(msg *Message) {
	if msg.Epoch == nil {
		return
	}
	epoch := *msg.Epoch
	if !c.setEpoch(epoch) {
		return
	}
	// Read the callback under the lock but call it outside, so a callback that
	// reads epoch() back cannot deadlock.
	c.epochMu.Lock()
	cb := c.onEpochChange
	c.epochMu.Unlock()
	if cb != nil {
		cb(epoch)
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
// goroutines notify at once — a gap or an inversion there would look like a
// dropped frame to the broker's §2.7 gap detection and force a resync.
func (c *Client) Notify(method string, params any) error {
	return c.codec.Notify(method, params)
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	return json.Marshal(params)
}
