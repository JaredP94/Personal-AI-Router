// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
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

// brokerEpochMethod announces this broker's protocol epoch to a client that
// never asked for it. The envelope already carries the epoch on every
// notification, but a client has no way to discover a value that only ever
// appears inside a frame it must already be reading to have any stream at all,
// so this pushes it as a first-class notification.
//
// The payload is the same ResyncResult broker:resync replies with — one shape,
// two delivery mechanisms — so a client parses the epoch identically whichever
// it gets. It rides on a NOTIFICATION, which is the only frame kind validate()
// accepts an envelope on; stamping it onto the resync REPLY would make every
// conforming client drop that reply as a terminal *ProtocolMismatchError.
const brokerEpochMethod = "broker:epoch"

const (
	// heartbeatShutdownJoinTimeout bounds the broker's own heartbeat writer join
	// when shutdown finds a client write blocked below the codec. It is kept to
	// one minWorkerGrace (250ms), far inside the broker's 10s teardownBudget that
	// the parent-side supervisor grace must cover, so stdio shutdown cannot spend
	// the process's kill grace on a pipe Close cannot unblock.
	heartbeatShutdownJoinTimeout = minWorkerGrace
)

// runHeartbeat emits one "heartbeat" notification per interval until ctx is
// cancelled, then returns. It also announces the broker epoch on the first tick
// after start, and again only if the epoch ever differs from the last one
// announced.
//
// There is exactly one ticker per Broker, and a Broker is one client
// connection, so a client sees one heartbeat stream with one seq run. The
// heartbeat frame carries no params: after spec §2.1 the client's only need is
// liveness plus the envelope, and the envelope is stamped by Codec.Notify, not
// by hand here. The epoch rides alongside it on its own frame so that neither
// notification has to grow a params field it does not use.
func (b *Broker) startHeartbeat(ctx context.Context, cancel context.CancelFunc) func() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.runHeartbeat(ctx)
	}()
	return func() {
		cancel()
		b.closeClientTransport()
		select {
		case <-done:
		case <-time.After(heartbeatShutdownJoinTimeout):
			log.Printf("abandoning heartbeat shutdown join after %s because client transport could not be unblocked", heartbeatShutdownJoinTimeout)
		}
	}
}

func (b *Broker) closeClientTransport() {
	if b.closeTransport == nil {
		return
	}
	if err := b.closeTransport(); err != nil {
		log.Printf("failed to close client transport during heartbeat shutdown: %v", err)
	}
}

func (b *Broker) notifyWithShutdown(ctx context.Context, method string, params any) error {
	done := make(chan error, 1)
	go func() { done <- b.codec.Notify(method, params) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		b.closeClientTransport()
		select {
		case err := <-done:
			return err
		case <-time.After(heartbeatShutdownJoinTimeout):
			return fmt.Errorf("abandoned %s write after %s because client transport could not be unblocked", method, heartbeatShutdownJoinTimeout)
		}
	}
}

func (b *Broker) runHeartbeat(ctx context.Context) {
	interval := b.heartbeatEvery()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// The epoch last announced on this connection. Empty until the first tick,
	// so the first tick always counts as a change: a client that connects after
	// startup learns the epoch within one interval rather than having to ask
	// for it. jsonrpc.Epoch() is stable for the life of the process, so over a
	// single connection this fires exactly once; the re-emit covers the case
	// where a Broker is rebuilt for a fresh connection.
	announced := ""
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
			//
			// The epoch goes out BEFORE the heartbeat so a client that reads
			// the stream in order has the epoch in hand before liveness.
			if epoch := jsonrpc.Epoch(); epoch != announced {
				if err := b.notifyWithShutdown(ctx, brokerEpochMethod, ResyncResult{
					Epoch:    epoch,
					Protocol: jsonrpc.ProtocolVersion,
				}); err != nil {
					log.Printf("failed to emit %s: %v", brokerEpochMethod, err)
					// Same contract as the heartbeat below: the read loop owns
					// teardown, so stop emitting rather than spin on a dead
					// writer.
					return
				}
				announced = epoch
			}
			if err := b.notifyWithShutdown(ctx, "heartbeat", map[string]any{}); err != nil {
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
