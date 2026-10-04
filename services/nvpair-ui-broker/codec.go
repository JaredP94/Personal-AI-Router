// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// The newline-delimited JSON-RPC 2.0 codec and the bidirectional Peer
// (id-correlation + read pump) are single-sourced in nvpair-shared/jsonrpc. These
// local aliases keep the broker's call sites unchanged. This covers the
// broker's client-facing Codec and the parent-side worker handles, which are
// built on Peer.

import (
	"io"

	"nvpair-shared/jsonrpc"
)

type (
	Message  = jsonrpc.Message
	RPCError = jsonrpc.RPCError
	Codec    = jsonrpc.Codec
	Peer     = jsonrpc.Peer
)

var (
	NewCodec = jsonrpc.NewCodec
	NewPeer  = jsonrpc.NewPeer
)

// errPeerClosed is the sentinel a worker handle's Call/RelayRequest returns
// once the child's transport has gone away.
var errPeerClosed = jsonrpc.ErrPeerClosed

// readWriter adapts a child's separate stdout (read) and stdin (write) into the
// io.ReadWriter the shared Codec expects.
type readWriter struct {
	io.Reader
	io.Writer
}

// writeOnlyReadWriter adapts a write-only child stream — a stdin pipe — into the
// io.ReadWriter the shared Codec expects. These handles only push control frames
// down a child's stdin and never read it: the child's stdout is drained by the
// handle's own reader, so handing the same reader to a second Codec would leave
// two scanners racing for the same bytes. Read therefore refuses immediately
// rather than blocking.
type writeOnlyReadWriter struct {
	io.Writer
}

func (writeOnlyReadWriter) Read([]byte) (int, error) { return 0, io.EOF }
