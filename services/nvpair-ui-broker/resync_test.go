// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"sort"
	"testing"
	"time"

	"nvpair-shared/jsonrpc"
)

// newResyncPipe returns a broker side wired to a client-side frame reader. The
// test reads frames on the returned *bufio.Scanner, so every frame the broker
// writes is drained by the test rather than blocking net.Pipe.
func newResyncPipe(t *testing.T) (*bufio.Scanner, net.Conn, net.Conn) {
	t.Helper()
	brokerSide, clientSide := net.Pipe()
	t.Cleanup(func() {
		_ = brokerSide.Close()
		_ = clientSide.Close()
	})
	return bufio.NewScanner(clientSide), brokerSide, clientSide
}

// readResyncFrame reads one wire frame as raw JSON object keys plus decoded
// values. It returns the top-level keys so a test can assert on the presence
// or absence of the envelope fields without printing frame bytes.
func readResyncFrame(t *testing.T, sc *bufio.Scanner, conn net.Conn, timeout time.Duration) (map[string]json.RawMessage, map[string]any) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if !sc.Scan() {
		t.Fatalf("scan frame: %v", sc.Err())
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear read deadline: %v", err)
	}
	rawKeys := map[string]json.RawMessage{}
	if err := json.Unmarshal(sc.Bytes(), &rawKeys); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	decoded := map[string]any{}
	if err := json.Unmarshal(sc.Bytes(), &decoded); err != nil {
		t.Fatalf("decode frame values: %v", err)
	}
	return rawKeys, decoded
}

// TestResyncResponseCarriesBrokerEpochWithoutEnvelopeStamp pins the one rule
// that makes broker:resync work on protocol v2. broker:resync is a REQUEST, so
// its reply travels the response path, which validate() holds to carrying no
// epoch and no seq — a stamped reply would be rejected as a
// *ProtocolMismatchError and read as a terminal mismatch. The broker's epoch
// reaches the client inside the result object instead.
func TestResyncResponseCarriesBrokerEpochWithoutEnvelopeStamp(t *testing.T) {
	sc, brokerSide, conn := newResyncPipe(t)
	b := &Broker{codec: NewCodec(brokerSide), heartbeatInterval: jsonrpc.HeartbeatInterval}

	id := json.RawMessage(`1`)
	respondErr := make(chan error, 1)
	go func() { respondErr <- b.handleResync(&id) }()

	keys, values := readResyncFrame(t, sc, conn, 2*time.Second)
	if err := <-respondErr; err != nil {
		t.Fatalf("handleResync: %v", err)
	}

	for _, banned := range []string{"epoch", "seq"} {
		if _, ok := keys[banned]; ok {
			t.Fatalf("broker:resync response carries envelope field %q; keys present: %v", banned, keyNames(keys))
		}
	}
	if values["jsonrpc"] != "2.0" {
		t.Fatalf("response jsonrpc = %v, want 2.0", values["jsonrpc"])
	}
	if got, ok := values["id"]; !ok || got != float64(1) {
		t.Fatalf("response id = %v (present=%v), want 1", got, ok)
	}

	rawResult, ok := keys["result"]
	if !ok {
		t.Fatalf("broker:resync response has no result; keys present: %v", keyNames(keys))
	}
	var result ResyncResult
	if err := json.Unmarshal(rawResult, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.Epoch == "" {
		t.Fatal("resync result epoch is empty; it must never reach the wire empty")
	}
	if result.Epoch != jsonrpc.Epoch() {
		t.Fatalf("resync result epoch = %q, want the broker process epoch %q", result.Epoch, jsonrpc.Epoch())
	}
	if result.Protocol != jsonrpc.ProtocolVersion {
		t.Fatalf("resync result protocol = %d, want %d", result.Protocol, jsonrpc.ProtocolVersion)
	}
}

// TestHeartbeatEmitsStampedNotificationsUntilContextCancel proves liveness and
// stopping behaviourally rather than by reading a log: a short injected
// interval must put real "heartbeat" notifications on the wire, each carrying
// this connection's epoch and a strictly increasing seq, and cancelling ctx
// must return the goroutine and leave the connection silent.
func TestHeartbeatEmitsStampedNotificationsUntilContextCancel(t *testing.T) {
	sc, brokerSide, conn := newResyncPipe(t)
	b := &Broker{codec: NewCodec(brokerSide), heartbeatInterval: 5 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.runHeartbeat(ctx)
	}()

	var lastSeq float64
	for i := 0; i < 3; i++ {
		keys, values := readResyncFrame(t, sc, conn, 2*time.Second)
		if values["method"] != "heartbeat" {
			t.Fatalf("frame %d method = %v, want heartbeat", i, values["method"])
		}
		// A notification frame is the only frame validate() accepts a stamp on.
		epoch, ok := keys["epoch"]
		if !ok {
			t.Fatalf("heartbeat %d carries no epoch; keys present: %v", i, keyNames(keys))
		}
		var epochVal string
		if err := json.Unmarshal(epoch, &epochVal); err != nil {
			t.Fatalf("decode heartbeat epoch: %v", err)
		}
		if epochVal == "" {
			t.Fatalf("heartbeat %d carries an empty epoch", i)
		}
		if epochVal != jsonrpc.Epoch() {
			t.Fatalf("heartbeat %d epoch = %q, want the broker process epoch %q", i, epochVal, jsonrpc.Epoch())
		}
		rawSeq, ok := keys["seq"]
		if !ok {
			t.Fatalf("heartbeat %d carries no seq; keys present: %v", i, keyNames(keys))
		}
		var seq float64
		if err := json.Unmarshal(rawSeq, &seq); err != nil {
			t.Fatalf("decode heartbeat seq: %v", err)
		}
		if seq <= lastSeq {
			t.Fatalf("heartbeat %d seq = %v, want strictly greater than %v", i, seq, lastSeq)
		}
		lastSeq = seq
		if _, hasID := keys["id"]; hasID {
			t.Fatalf("heartbeat %d is not a notification: it carries an id", i)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runHeartbeat did not return within 2s of context cancel")
	}

	// Silence after stop is part of the contract: a heartbeat that kept ticking
	// past cancel would show up here.
	if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if sc.Scan() {
		t.Fatalf("runHeartbeat wrote a frame after ctx cancel: keys present: %v", keyNames(decodeFrameKeys(t, sc.Bytes())))
	} else if err := sc.Err(); err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
		// Silence past the deadline is the expected result; io.EOF and the
		// deadline timeout both mean "nothing more arrived".
		t.Fatalf("unexpected read error after ctx cancel: %v", err)
	}
}

// TestStampedResponseIsRejectedAndStampedNotificationIsAccepted is the direct
// proof of the envelope rule this feature sits on, run through the same
// validate() the client's read loop uses. It is what makes the resync test
// above meaningful: if a stamped reply were legal, handleResync could stamp
// and nothing would notice.
func TestStampedResponseIsRejectedAndStampedNotificationIsAccepted(t *testing.T) {
	cases := []struct {
		name       string
		frame      string
		wantReject bool
	}{
		{
			name:       "stamped response is rejected",
			frame:      `{"jsonrpc":"2.0","id":1,"result":{"epoch":"e","protocol":2},"epoch":"e","seq":1}`,
			wantReject: true,
		},
		{
			name:       "stamped error response is rejected",
			frame:      `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"x"},"epoch":"e","seq":1}`,
			wantReject: true,
		},
		{
			name:       "unstamped response is accepted",
			frame:      `{"jsonrpc":"2.0","id":1,"result":{"epoch":"e","protocol":2}}`,
			wantReject: false,
		},
		{
			name:       "stamped notification is accepted",
			frame:      `{"jsonrpc":"2.0","method":"heartbeat","params":{},"epoch":"e","seq":1}`,
			wantReject: false,
		},
		{
			name:       "unstamped notification is rejected",
			frame:      `{"jsonrpc":"2.0","method":"heartbeat","params":{}}`,
			wantReject: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader, writer := net.Pipe()
			t.Cleanup(func() {
				_ = reader.Close()
				_ = writer.Close()
			})
			go func() {
				_, _ = io.WriteString(writer, tc.frame+"\n")
			}()
			codec := NewCodec(reader)
			_ = reader.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, err := codec.Read()
			if tc.wantReject {
				var pm *jsonrpc.ProtocolMismatchError
				if !errors.As(err, &pm) {
					t.Fatalf("Read error = %v (%T), want *ProtocolMismatchError", err, err)
				}
				// Terminal by design: it must not be mistaken for a
				// recoverable *DecodeError.
				var de *jsonrpc.DecodeError
				if errors.As(err, &de) {
					t.Fatal("mismatch error must not be a *DecodeError")
				}
				return
			}
			if err != nil {
				t.Fatalf("Read error = %v, want the frame accepted", err)
			}
		})
	}
}

// keyNames returns the sorted key set of a decoded frame, for failure messages
// that name the offending field without echoing frame bytes.
func keyNames(keys map[string]json.RawMessage) []string {
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// decodeFrameKeys decodes a raw frame's top-level keys.
func decodeFrameKeys(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	keys := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	return keys
}
