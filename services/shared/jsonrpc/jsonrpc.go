// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package jsonrpc is the shared newline-delimited JSON-RPC 2.0 codec used by
// every NVPAIR subprocess over stdio (or an --ipc socket/pipe).
//
// The codec owns newline framing, protocol-v2 envelopes and a bounded ordered
// output queue. Notify and responses return acceptance, not guaranteed delivery;
// Flush/Drain/Close establish delivery barriers. Peer adds request correlation.
// See README.md for transport ownership, sizing measurements and shutdown rules.
package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync/atomic"
)

// Message is a single JSON-RPC 2.0 frame. A frame is a request (id+method), a
// notification (method, no id), or a response (id, no method) — see the
// IsRequest/IsNotification/IsResponse classifiers.
type Message struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *RPCError        `json:"error,omitempty"`

	// Protocol v2: present on every notification, absent on requests and
	// responses. Their presence IS the version discriminator (§2.2).
	Epoch *string `json:"epoch,omitempty"`
	Seq   *uint64 `json:"seq,omitempty"`
}

// RPCError is the JSON-RPC 2.0 error object. It is both the wire shape and a
// Go error, so a caller that gets one back from a failed Call can return it
// directly without re-wrapping code and message.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// DecodeError marks a recoverable per-frame decode failure: the scanner
// advanced past the bad line, so the next Read can continue. Terminal
// transport/scanner errors are returned as plain errors instead, so a read
// loop stops rather than spinning on a permanently-failing read. Callers that
// want to continue-on-bad-frame can type-assert / errors.As for *DecodeError.
type DecodeError struct{ Err error }

func (e *DecodeError) Error() string { return e.Err.Error() }
func (e *DecodeError) Unwrap() error { return e.Err }

// IsRequest reports whether the frame is an id-bearing request.
func (m *Message) IsRequest() bool {
	return m.ID != nil && m.Method != ""
}

// IsNotification reports whether the frame is a fire-and-forget notification.
func (m *Message) IsNotification() bool {
	return m.ID == nil && m.Method != ""
}

// IsResponse reports whether the frame is a response to a request (id, no
// method). Used by the parent-side demux and any bidirectional peer.
func (m *Message) IsResponse() bool {
	return m.ID != nil && m.Method == ""
}

// Codec handles newline-delimited JSON-RPC 2.0 over an io.ReadWriter.
// Writes are safe for concurrent use.
type Codec struct {
	scanner *bufio.Scanner
	writer  io.Writer
	out     orderedWriter

	// seq is this connection's notification counter, starting at 1. The broker
	// builds one Codec per client connection, so it is already per-connection.
	seq atomic.Uint64
}

// NewCodec wraps rw with a Codec. Reads accept frames up to 1 MiB.
func NewCodec(rw io.ReadWriter) *Codec {
	scanner := bufio.NewScanner(rw)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	c := &Codec{scanner: scanner, writer: rw}
	c.initWriter()
	return c
}

// NewCodecMaxFrame is like NewCodec but caps a single inbound frame at
// maxFrame bytes (initial buffer 64 KiB, grown on demand). Used by peers that
// exchange larger frames than the 1 MiB default (e.g. nvpair-engine-manager). An
// inbound frame larger than maxFrame surfaces as a terminal scanner read error
// (bufio.Scanner cannot resync past an over-long line), so the read loop exits
// cleanly rather than spinning.
func NewCodecMaxFrame(rw io.ReadWriter, maxFrame int) *Codec {
	scanner := bufio.NewScanner(rw)
	scanner.Buffer(make([]byte, 0, 64*1024), maxFrame)
	c := &Codec{scanner: scanner, writer: rw}
	c.initWriter()
	return c
}

// Read returns the next frame, or io.EOF when the stream closes. A malformed
// frame (bad JSON or wrong version) is returned as a recoverable *DecodeError;
// a protocol v2 envelope violation is returned as a *ProtocolMismatchError,
// which is terminal; a terminal scanner/transport error is returned as a plain
// error.
func (c *Codec) Read() (*Message, error) {
	select {
	case <-c.Done():
		return nil, io.EOF
	default:
	}
	if !c.scanner.Scan() {
		select {
		case <-c.Done():
			return nil, io.EOF
		default:
		}
		if err := c.scanner.Err(); err != nil {
			return nil, fmt.Errorf("read error: %w", err)
		}
		return nil, io.EOF
	}
	line := c.scanner.Bytes()
	if err := checkCanonical(line); err != nil {
		return nil, err
	}
	var msg Message
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil, &DecodeError{fmt.Errorf("invalid JSON-RPC message: %w", err)}
	}
	if msg.JSONRPC != "2.0" {
		return nil, &DecodeError{fmt.Errorf("unsupported JSON-RPC version: %q", msg.JSONRPC)}
	}
	// Returned unwrapped: a mismatch means the peer does not speak protocol v2,
	// and *DecodeError is documented as recoverable, so wrapping it here would
	// let the read loop continue on a connection it can never make sense of.
	if err := validate(&msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// Notify accepts a fire-and-forget notification (method, params, no id).
// It waits at most WriteAdmissionWait for delivery, then returns acceptance.
// Use Flush or Close when delivery must precede shutdown.
func (c *Codec) Notify(method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	msg := &Message{
		JSONRPC: "2.0",
		Method:  method,
		Params:  raw,
	}
	return c.writeNotification(msg)
}

// Request sends an id-bearing request. Unlike Notify it carries no envelope
// stamp: validate() rejects epoch/seq on anything that is not a notification, so
// stamping a request would make the frame unreadable to a v2 peer. Peer.Call is
// the higher-level form of this for callers that also want the response.
func (c *Codec) Request(id *json.RawMessage, method string, params json.RawMessage) error {
	return c.request(context.Background(), id, method, params)
}

// request accepts an unstamped request into the same ordered writer queue.
func (c *Codec) request(ctx context.Context, id *json.RawMessage, method string, params json.RawMessage) error {
	return c.enqueue(ctx, &Message{JSONRPC: "2.0", ID: id, Method: method, Params: params})
}

// Respond sends a successful response carrying result for the given request id.
func (c *Codec) Respond(id *json.RawMessage, result any) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return c.write(&Message{
		JSONRPC: "2.0",
		ID:      id,
		Result:  raw,
	})
}

// RespondError sends an error response for the given request id.
func (c *Codec) RespondError(id *json.RawMessage, code int, message string) error {
	return c.write(&Message{
		JSONRPC: "2.0",
		ID:      id,
		Error: &RPCError{
			Code:    code,
			Message: message,
		},
	})
}

// RespondErrorData is like RespondError but carries an optional structured
// data payload (JSON-RPC 2.0 error object's "data" field). data may be nil.
func (c *Codec) RespondErrorData(id *json.RawMessage, code int, message string, data any) error {
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return err
		}
		raw = b
	}
	return c.write(&Message{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &RPCError{Code: code, Message: message, Data: raw},
	})
}
