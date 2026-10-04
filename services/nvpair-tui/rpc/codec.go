// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package rpc is the client half of the newline-delimited JSON-RPC 2.0
// protocol every NVPAIR subprocess speaks. The TUI uses it to drive
// nvpair-ui-broker: one JSON object per line, requests carry a numeric
// id and expect a matching response, notifications carry a method but no
// id and are pushed without a reply.
//
// The framing, the wire types and the protocol v2 envelope are NOT
// defined here: they live in nvpair-shared/jsonrpc, the same codec the
// broker writes with, so the two sides cannot drift. What remains in
// this package is the part that is genuinely TUI-side — outbound id
// allocation, response correlation, and the notification fan-out the UI
// reads. In particular Client.Notify goes through jsonrpc.Codec.Notify,
// which stamps every notification with the epoch and seq a v2 reader
// requires; there is no second notification emitter on this wire.
package rpc

import (
	"io"

	"nvpair-shared/jsonrpc"
)

type (
	// Message and RPCError are the shared wire types, aliased so the UI keeps
	// naming the very frame the broker sends. Message now carries the
	// protocol v2 envelope fields (Epoch, Seq), present on notifications
	// and absent on requests and responses.
	Message  = jsonrpc.Message
	RPCError = jsonrpc.RPCError
	Codec    = jsonrpc.Codec
)

// NewCodec wraps a reader/writer pair (the broker's stdout/stdin) in the
// canonical framing codec.
func NewCodec(rw io.ReadWriter) *Codec { return jsonrpc.NewCodec(rw) }

// readWriter adapts the broker's separate stdout (read) and stdin (write) into
// the io.ReadWriter NewCodec expects. The framing is 1 MiB, matching the
// broker's read buffer, so a large discovery/errors snapshot is never truncated
// mid-frame.
type readWriter struct {
	io.Reader
	io.Writer
}
