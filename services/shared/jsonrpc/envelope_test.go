// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package jsonrpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
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
