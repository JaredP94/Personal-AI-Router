// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package jsonrpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestNotifyStampsEpochAndSeq(t *testing.T) {
	var buf bytes.Buffer
	c := writeCodec(&buf)
	if err := c.Notify("node/updated", map[string]string{"id": "n1"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if err := c.Notify("node/updated", map[string]string{"id": "n2"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 frames, got %d", len(lines))
	}
	// seq starts at 1 and increases by exactly 1 per notification.
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
		if frame.Epoch == nil || *frame.Epoch != Epoch() {
			t.Errorf("frame %d epoch = %v, want %s", i, frame.Epoch, Epoch())
		}
	}
}

func TestSeqIsPerCodecNotGlobal(t *testing.T) {
	if got := Epoch(); got == "" {
		t.Fatal("Epoch() is empty")
	}
	var a, b bytes.Buffer
	ac, bc := writeCodec(&a), writeCodec(&b)
	for range 3 {
		if err := ac.Notify("m", nil); err != nil {
			t.Fatalf("ac.Notify: %v", err)
		}
	}
	if err := bc.Notify("m", nil); err != nil {
		t.Fatalf("bc.Notify: %v", err)
	}
	// Each codec numbers its own connection from 1, so the second codec's
	// single frame must carry seq 1 even though the first is at seq 3.
	if want := `"seq":1`; !strings.Contains(b.String(), want) {
		t.Errorf("second connection: want %s, got %s", want, b.String())
	}
}

func TestStampingOmitsResponsesAndRequests(t *testing.T) {
	var buf bytes.Buffer
	c := writeCodec(&buf)
	id := json.RawMessage("7")
	if err := c.Respond(&id, map[string]bool{"ok": true}); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, `"epoch"`) || strings.Contains(out, `"seq"`) {
		t.Errorf("response frame must not carry epoch/seq: %s", out)
	}
}

func TestReadRejectsUnstampedNotification(t *testing.T) {
	// A v1 peer: valid JSON-RPC, no envelope stamp.
	c := readCodec(strings.NewReader(`{"jsonrpc":"2.0","method":"ready","params":{}}` + "\n"))
	_, err := c.Read()
	var pm *ProtocolMismatchError
	if !errors.As(err, &pm) {
		t.Fatalf("want *ProtocolMismatchError, got %v (%T)", err, err)
	}
	if !strings.Contains(err.Error(), "epoch") {
		t.Errorf("error must name the missing member: %v", err)
	}
}

func TestReadRejectsStampOnResponse(t *testing.T) {
	c := readCodec(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{},"epoch":"e","seq":1}` + "\n"))
	_, err := c.Read()
	var pm *ProtocolMismatchError
	if !errors.As(err, &pm) {
		t.Fatalf("want *ProtocolMismatchError for stamped response, got %v (%T)", err, err)
	}
}

func TestEpochIsStableWithinProcess(t *testing.T) {
	first, second := Epoch(), Epoch()
	if first != second {
		t.Fatalf("Epoch() unstable: %q then %q", first, second)
	}
	if _, err := uuid.Parse(first); err != nil {
		t.Fatalf("Epoch() is not a UUID: %q (%v)", first, err)
	}
}

// TestConcurrentNotifyWritesInSeqOrder pins the wire-order contract: the seq a
// peer reads must equal its frame's position on the wire, with no gaps and no
// repeats. Stamping outside the write lock inverted seq under concurrency —
// goroutine A stamped seq 1, B stamped seq 2, B won wmu — violating the sender's
// monotonic wire guarantee. Receiver recovery is epoch-only, never gap-based.
//
// Run with -count=20: the interleaving that broke the old code is rare, so a
// single green run proves little.
func TestConcurrentNotifyWritesInSeqOrder(t *testing.T) {
	const (
		goroutines    = 16
		perGoroutine  = 8
		totalFrames   = goroutines * perGoroutine
		wantMinFrames = 100
	)
	if totalFrames < wantMinFrames {
		t.Fatalf("test needs at least %d frames to be meaningful, got %d", wantMinFrames, totalFrames)
	}

	var buf syncBuffer
	c := writeCodec(&buf)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release every goroutine at once to maximise overlap
			for i := range perGoroutine {
				if err := c.Notify("m", map[string]int{"g": g, "i": i}); err != nil {
					t.Errorf("Notify: %v", err)
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	// Decode every frame off the wire in order and assert wire order == seq order.
	reader := readCodec(bytes.NewReader(buf.Bytes()))
	for want := uint64(1); want <= uint64(totalFrames); want++ {
		msg, err := reader.Read()
		if err != nil {
			t.Fatalf("reading the frame stamped seq %d: %v", want, err)
		}
		if !msg.IsNotification() {
			t.Fatalf("wire position %d: expected a notification, got %+v", want, msg)
		}
		if msg.Seq == nil {
			t.Fatalf("wire position %d: frame carries no seq", want)
		}
		if *msg.Seq != want {
			t.Fatalf("wire order != seq order: frame at wire position %d carries seq %d", want, *msg.Seq)
		}
		if msg.Epoch == nil || *msg.Epoch != Epoch() {
			t.Fatalf("frame with seq %d carries the wrong epoch", want)
		}
	}
	if _, err := reader.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected exactly %d frames then EOF, got err %v", totalFrames, err)
	}
}

// syncBuffer is an io.Writer safe to hand to a Codec that concurrent
// goroutines notify through. The codec serialises its own writes with wmu, but
// the test does not assume that and the buffer is cheap to make safe.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}
