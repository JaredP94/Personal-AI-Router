// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package jsonrpc

import (
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ProtocolVersion is the wire protocol version this codec speaks. There is no
// negotiation: the envelope stamp (§2.2) is the discriminator.
const ProtocolVersion = 2

// ReservedMembers is the set of top-level JSON-RPC envelope member names reserved
// by protocol v2.
var ReservedMembers = []string{"jsonrpc", "id", "method", "params", "result", "error", "epoch", "seq"}

// HeartbeatInterval is how often a peer expects to hear from the other side.
const HeartbeatInterval = 15 * time.Second

// HeartbeatTimeout is how long a channel may stay silent before the
// connection is presumed dead and the reader reconnects (a reconnect starts a
// fresh seq run, so the client resyncs).
const HeartbeatTimeout = 3 * HeartbeatInterval

var (
	epochOnce sync.Once
	epochVal  string
)

// Epoch returns this process's protocol epoch: a UUID minted on first call and
// stable for the life of the process. A client that sees a different epoch
// knows the peer restarted and every cached fact it holds is suspect.
func Epoch() string {
	epochOnce.Do(func() { epochVal = uuid.NewString() })
	return epochVal
}

// ProtocolMismatchError reports a relay-protocol envelope violation: the peer
// does not speak protocol v2. It is deliberately NOT a *DecodeError — a bad
// frame is recoverable and the read loop continues, but a mismatch means the
// two sides disagree about the wire format, so the connection is terminal.
type ProtocolMismatchError struct{ Msg string }

func (e *ProtocolMismatchError) Error() string { return "protocol mismatch: " + e.Msg }

// stamp assigns this connection's next seq and the process epoch to a
// notification frame. Callers never do this themselves: Notify stamps every
// notification it writes, so no call site has to know the envelope exists.
// stamp is called with wmu held so that the seq it assigns is the seq that
// reaches the wire next; seq is atomic purely for the race detector.
func (c *Codec) stamp(msg *Message) {
	epoch := Epoch()
	seq := c.seq.Add(1)
	msg.Epoch = &epoch
	msg.Seq = &seq
}

// validate enforces the one protocol v2 rule: every notification carries epoch
// and seq, and nothing else does. Their absence (or their appearance on a
// request or response) means the peer predates or misunderstands the envelope.
// Frames that are not notifications carry no stamp and are left alone; every
// other concern stays a *DecodeError at the call site in Read.
func validate(msg *Message) error {
	if msg.IsNotification() {
		if msg.Epoch == nil || msg.Seq == nil {
			return &ProtocolMismatchError{fmt.Sprintf("notification without epoch/seq (method %q)", msg.Method)}
		}
		return nil
	}
	if msg.Epoch != nil || msg.Seq != nil {
		return &ProtocolMismatchError{fmt.Sprintf("epoch/seq on a non-notification frame (method %q)", msg.Method)}
	}
	return nil
}
