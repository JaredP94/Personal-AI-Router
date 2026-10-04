// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package applog

import (
	"bytes"
	"encoding/json"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"

	"nvpair-shared/jsonrpc"
)

// TestNotifyStampsProtocolV2Envelope pins the stamp on the frames this emitter
// puts on the wire. The broker drains node-info's stdout and applies the same
// protocol v2 rule as the shared codec, so an unstamped notification is a frame
// no v2 reader will accept — and nothing in this package would notice, because
// StdinRPC only reads, never writes, through the stamped path.
func TestNotifyStampsProtocolV2Envelope(t *testing.T) {
	var buf bytes.Buffer
	n := NewNotifier(&buf)
	if err := n.Notify("nodeinfo:observed-addresses", map[string][]string{"addresses": {"10.0.0.5"}}); err != nil {
		t.Fatalf("notify: %v", err)
	}
	// A nil params must still be omitted, exactly as before the stamp: the
	// method, not the payload, is what these reports are for.
	if err := n.Notify("ready", nil); err != nil {
		t.Fatalf("notify with nil params: %v", err)
	}

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("emitted %d frames, want 2", len(lines))
	}
	for i, wantSeq := range []uint64{1, 2} {
		var frame struct {
			Epoch *string `json:"epoch"`
			Seq   *uint64 `json:"seq"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &frame); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if frame.Seq == nil || *frame.Seq != wantSeq {
			t.Errorf("frame %d seq = %v, want %d", i, frame.Seq, wantSeq)
		}
		if frame.Epoch == nil || *frame.Epoch != jsonrpc.Epoch() {
			t.Errorf("frame %d epoch = %v, want %s", i, frame.Epoch, jsonrpc.Epoch())
		}
	}
	if strings.Contains(lines[1], `"params"`) {
		t.Errorf("nil params must stay off the wire: %s", lines[1])
	}

	// The point of the stamp: the canonical reader accepts every frame.
	reader := jsonrpc.NewCodec(readerWriter{Reader: strings.NewReader(buf.String()), Writer: io.Discard})
	for i := range 2 {
		msg, err := reader.Read()
		if err != nil {
			t.Fatalf("canonical reader rejected frame %d: %v", i, err)
		}
		if !msg.IsNotification() {
			t.Errorf("frame %d is not a notification: %+v", i, msg)
		}
	}
}

// TestNotifyWritesInSeqOrderUnderConcurrency pins the wire-order contract for the
// hand-rolled stamp: the seq a peer reads must equal its frame's position on the
// wire, with no gaps and no repeats. Assigning seq outside mu would let two
// concurrent Notify calls invert — A takes 1, B takes 2, B wins the lock — which
// §2.7 gap detection reads as a dropped frame and answers with a spurious resync.
func TestNotifyWritesInSeqOrderUnderConcurrency(t *testing.T) {
	const (
		notifiers   = 16
		perWriter   = 8
		totalFrames = notifiers * perWriter
	)
	var buf lockedBuffer
	n := NewNotifier(&buf)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := range notifiers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release every writer at once to maximise overlap
			for i := range perWriter {
				if err := n.Notify("nodeinfo:observed-addresses", map[string]int{"w": w, "i": i}); err != nil {
					t.Errorf("notify: %v", err)
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	reader := jsonrpc.NewCodec(readerWriter{Reader: bytes.NewReader(buf.Bytes()), Writer: io.Discard})
	for want := uint64(1); want <= totalFrames; want++ {
		msg, err := reader.Read()
		if err != nil {
			t.Fatalf("reading the frame stamped seq %d: %v", want, err)
		}
		if msg.Seq == nil || *msg.Seq != want {
			t.Fatalf("wire order != seq order: the frame at wire position %d carries seq %v", want, msg.Seq)
		}
		if msg.Epoch == nil || *msg.Epoch != jsonrpc.Epoch() {
			t.Fatalf("frame with seq %d carries the wrong epoch", want)
		}
	}
	if _, err := reader.Read(); err != io.EOF {
		t.Fatalf("expected exactly %d frames then EOF, got err %v", totalFrames, err)
	}
}

// readerWriter adapts a reader and a writer into the io.ReadWriter NewCodec wants.
type readerWriter struct {
	io.Reader
	io.Writer
}

// lockedBuffer is safe to hand to a Notifier that several goroutines notify
// through. The Notifier serializes its own writes, but the test does not assume
// that and the buffer is cheap to make safe.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

// TestNotifyEmitsOneNewlineTerminatedNotification pins the wire form. A parent
// reads this stream as newline-delimited JSON-RPC, so a missing newline or a
// second frame's worth of bytes on one line stops it decoding anything further.
func TestNotifyEmitsOneNewlineTerminatedNotification(t *testing.T) {
	var buf bytes.Buffer
	params := map[string][]string{"addresses": {"10.172.54.70", "10.0.0.5"}}
	if err := NewNotifier(&buf).Notify("nodeinfo:observed-addresses", params); err != nil {
		t.Fatalf("notify: %v", err)
	}

	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("frame is not newline-terminated: %q", out)
	}
	if got := strings.Count(out, "\n"); got != 1 {
		t.Errorf("emitted %d newlines, want exactly one frame", got)
	}

	var frame struct {
		JSONRPC string              `json:"jsonrpc"`
		Method  string              `json:"method"`
		ID      json.RawMessage     `json:"id"`
		Params  map[string][]string `json:"params"`
	}
	if err := json.Unmarshal([]byte(out), &frame); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if frame.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want 2.0", frame.JSONRPC)
	}
	if frame.Method != "nodeinfo:observed-addresses" {
		t.Errorf("method = %q, want nodeinfo:observed-addresses", frame.Method)
	}
	// A notification carries no id: an id would make the parent's reader wait for
	// a reply to a report nobody asked for.
	if len(frame.ID) != 0 {
		t.Errorf("frame carries an id (%s), want a notification", frame.ID)
	}
	if got := frame.Params["addresses"]; len(got) != 2 || got[0] != "10.172.54.70" || got[1] != "10.0.0.5" {
		t.Errorf("params addresses = %v, want the two reported addresses", got)
	}
}

// A subprocess with no stdout channel gets a nil Notifier, and reporting must stay
// a plain call at every site rather than a nil check per caller.
func TestNilNotifierDropsFramesAndReportsSuccess(t *testing.T) {
	var n *Notifier

	if err := n.Notify("nodeinfo:observed-addresses", map[string][]string{"addresses": {"10.0.0.5"}}); err != nil {
		t.Errorf("nil Notifier.Notify returned %v, want the frame dropped silently", err)
	}
	frame := []byte(`{"jsonrpc":"2.0","id":1,"result":{"level":"debug"}}` + "\n")
	got, err := n.Write(frame)
	if err != nil {
		t.Errorf("nil Notifier.Write returned %v, want the frame dropped silently", err)
	}
	// The full length: a short write is an error to an io.Writer's caller, and
	// StdinRPC's response path would report a failure that did not happen.
	if got != len(frame) {
		t.Errorf("nil Notifier.Write wrote %d, want %d", got, len(frame))
	}
}

// splitWriter forwards each write to buf in two halves with a scheduling point
// between them. Without the Notifier's lock another writer lands in that gap and
// the two frames interleave; against a plain buffer, missing serialization would
// usually pass unnoticed with the race detector unavailable.
type splitWriter struct{ buf *bytes.Buffer }

func (w splitWriter) Write(p []byte) (int, error) {
	half := len(p) / 2
	if _, err := w.buf.Write(p[:half]); err != nil {
		return 0, err
	}
	runtime.Gosched()
	if _, err := w.buf.Write(p[half:]); err != nil {
		return half, err
	}
	return len(p), nil
}

// TestNotifierSerializesConcurrentNotifyAndWrite covers why the Notifier exists
// at all: a subprocess reports observations from whichever goroutine noticed
// them, while StdinRPC writes control responses to the same stream. Two frames
// sharing a line are unrecoverable for the parent's line-delimited reader.
func TestNotifierSerializesConcurrentNotifyAndWrite(t *testing.T) {
	const writers = 8
	const framesPerWriter = 100

	var buf bytes.Buffer
	n := NewNotifier(splitWriter{buf: &buf})
	// The shape StdinRPC's response path hands to Write, already marshaled.
	response := []byte(`{"jsonrpc":"2.0","id":7,"result":{"level":"debug"}}` + "\n")

	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for range framesPerWriter {
				if w%2 == 0 {
					params := map[string][]string{"addresses": {"10.172.54.70", "10.0.0.5", "192.168.240.1"}}
					if err := n.Notify("nodeinfo:observed-addresses", params); err != nil {
						t.Errorf("notify from writer %d: %v", w, err)
						return
					}
					continue
				}
				if _, err := n.Write(response); err != nil {
					t.Errorf("write from writer %d: %v", w, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("stream does not end on a frame boundary: last 80 bytes = %q", tail(out, 80))
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != writers*framesPerWriter {
		t.Fatalf("read back %d frames, want %d", len(lines), writers*framesPerWriter)
	}
	for i, line := range lines {
		var frame struct {
			JSONRPC string `json:"jsonrpc"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("frame %d is not one complete JSON object (%v): %q", i, err, line)
		}
		if frame.JSONRPC != "2.0" {
			t.Fatalf("frame %d jsonrpc = %q, want 2.0", i, frame.JSONRPC)
		}
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
