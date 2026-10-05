// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"nvpair-shared/jsonrpc"
)

// ResyncResult is the broker:resync reply: the epoch the client must treat as
// current and the wire protocol version this broker speaks. Per relay protocol
// v2 §2.2 the epoch is also stamped on every notification, but a client needs it
// before it has seen a single notification (the reply to broker:resync may be
// the first frame of the connection), so it rides in the result object here.
type ResyncResult struct {
	Epoch    string `json:"epoch"`
	Protocol int    `json:"protocol"`
}

// handleResync answers broker:resync.
//
// The envelope trap: broker:resync is a REQUEST, and validate() in
// shared/jsonrpc rejects any epoch or seq that appears on a frame that is not
// a notification. So this reply must go out through Codec.Respond, which
// writes an unstamped frame. Stamping it here by hand would make a conforming
// client drop the reply as a terminal *ProtocolMismatchError.
func (b *Broker) handleResync(id *json.RawMessage) error {
	epoch := jsonrpc.Epoch()
	if epoch == "" {
		// Epoch() mints a UUID on first call and never returns empty; an empty
		// value here would put an empty epoch on the wire and every client
		// would treat it as "unknown", so fail the call rather than answer.
		log.Printf("broker:resync: refusing to answer with an empty epoch")
		return b.codec.RespondError(id, -32603, "broker epoch unavailable")
	}
	if err := b.codec.Respond(id, ResyncResult{
		Epoch:    epoch,
		Protocol: jsonrpc.ProtocolVersion,
	}); err != nil {
		return err
	}
	return nil
}

// runHeartbeat emits one "heartbeat" notification per interval until ctx is
// cancelled, then returns.
//
// There is exactly one ticker per Broker, and a Broker is one client
// connection, so a client sees one heartbeat stream with one seq run. The frame
// carries no params: after spec §2.1 the client's only need is liveness plus
// the envelope, and the envelope is stamped by Codec.Notify, not by hand here.
func (b *Broker) runHeartbeat(ctx context.Context) {
	interval := b.heartbeatEvery()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Notify is the stamping path: it stamps epoch and seq inside the
			// codec's write lock so the wire keeps seq order. Hand-writing the
			// frame and assigning seq here would race the broker's other
			// notifications and could invert them (§2.7 gap detection reads an
			// inversion as a dropped frame).
			if err := b.codec.Notify("heartbeat", map[string]any{}); err != nil {
				log.Printf("failed to emit heartbeat: %v", err)
				// A write failure means the transport is gone. The read loop
				// owns connection teardown, so leave that to it and stop
				// emitting rather than spinning on a dead writer.
				return
			}
		}
	}
}

// heartbeatEvery is the heartbeat period, falling back to the shared default
// when the field is unset. A zero time.Duration would panic NewTicker, and a
// Broker built directly (as the tests do, or as NewBroker before this field
// existed) leaves it zero.
func (b *Broker) heartbeatEvery() time.Duration {
	if b.heartbeatInterval > 0 {
		return b.heartbeatInterval
	}
	return jsonrpc.HeartbeatInterval
}
