// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"

	"nvpair-shared/jsonrpc"
	"nvpair-shared/noderec"
)

// newNodeInfoHandle builds a node-info handle whose stdin is buf, with no child
// process behind it. Enough for the control-frame pushes, which only touch the
// write side.
func newNodeInfoHandle(buf io.Writer) *nodeInfoProcess {
	return &nodeInfoProcess{codec: NewCodec(writeOnlyReadWriter{buf})}
}

// TestWriteClusterIdentityFrame pins the wire form of the push node-info decodes.
// node-info reads its stdin as newline-delimited JSON-RPC, so a missing newline or
// a renamed method silently stops membership reaching it — and the only symptom
// would be peers going on suppressing an invite for a node that has left.
func TestWriteClusterIdentityFrame(t *testing.T) {
	for _, tc := range []struct {
		name        string
		clusterUUID string
	}{
		{"a principal", "our-principal"},
		// A departure is the value peers are waiting for, so it is sent like any
		// other rather than skipped as empty.
		{"a departure", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := newNodeInfoHandle(&buf).SetClusterIdentity(tc.clusterUUID); err != nil {
				t.Fatalf("write: %v", err)
			}

			line := buf.String()
			if !strings.HasSuffix(line, "\n") {
				t.Error("frame is not newline-terminated; node-info reads line-delimited frames")
			}
			if strings.Count(line, "\n") != 1 {
				t.Errorf("frame contains %d newlines, want exactly one", strings.Count(line, "\n"))
			}

			var frame struct {
				JSONRPC string                        `json:"jsonrpc"`
				Method  string                        `json:"method"`
				ID      json.RawMessage               `json:"id"`
				Params  noderec.ClusterIdentityParams `json:"params"`
			}
			if err := json.Unmarshal([]byte(line), &frame); err != nil {
				t.Fatalf("decode frame %q: %v", line, err)
			}
			if frame.JSONRPC != "2.0" {
				t.Errorf("jsonrpc = %q, want 2.0", frame.JSONRPC)
			}
			if frame.Method != noderec.MethodSetClusterIdentity {
				t.Errorf("method = %q, want %q", frame.Method, noderec.MethodSetClusterIdentity)
			}
			// A notification, not a request: node-info's stdout is drained to
			// io.Discard, so an id-bearing frame would strand a reply.
			if len(frame.ID) != 0 {
				t.Errorf("frame carries an id (%s); the push must be a notification", frame.ID)
			}
			if frame.Params.ClusterUUID != tc.clusterUUID {
				t.Errorf("clusterUuid = %q, want %q", frame.Params.ClusterUUID, tc.clusterUUID)
			}
			// The field must be on the wire even when empty, since that is how a
			// departure is expressed.
			if !strings.Contains(line, `"clusterUuid"`) {
				t.Errorf("frame omitted clusterUuid: %s", line)
			}
			// Protocol v2: the notification carries the envelope, because
			// node-info's applog stdin reader — and any v2 reader behind it —
			// rejects one without.
			assertStamped(t, line)
		})
	}
}

// TestChildStdinFramesAreStamped covers both control pushes to node-info and
// both to the workload-manager. All four are notifications on the stdio envelope,
// so an unstamped one is a frame the child's v2 reader will not accept.
func TestChildStdinFramesAreStamped(t *testing.T) {
	for _, tc := range []struct {
		name string
		send func(io.Writer) error
	}{
		{"node-info set-cluster-identity", func(w io.Writer) error {
			return newNodeInfoHandle(w).SetClusterIdentity("c1")
		}},
		{"node-info log/set-level", func(w io.Writer) error {
			return newNodeInfoHandle(w).SetLogLevel("debug")
		}},
		{"workload-manager log/set-level", func(w io.Writer) error {
			m := &workloadManagerProcess{codec: NewCodec(writeOnlyReadWriter{w})}
			return m.SetLogLevel("debug")
		}},
		{"workload-manager forward", func(w io.Writer) error {
			m := &workloadManagerProcess{codec: NewCodec(writeOnlyReadWriter{w})}
			return m.Forward(noderec.NotifyNodes, json.RawMessage(`{"nodes":[]}`))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := tc.send(&buf); err != nil {
				t.Fatalf("send: %v", err)
			}
			assertStamped(t, buf.String())
		})
	}
}

// TestNodeInfoControlFramesWriteInSeqOrder is the concurrent half of the node-info
// pushes. Two control paths share this handle — the broker's log-level broadcast
// and the membership watcher that pushes a new cluster principal — so they race in
// practice. The seq node-info reads must equal the frame's position on the wire:
// §2.7 gap detection reads an inversion as a dropped frame and answers with a
// spurious resync, and the two senders also share one pipe, so an interleaved
// frame would be undecodable on top of that.
//
// Run under -race: the point of routing both pushes through the handle's codec is
// that the stamp and the write share one critical section, and only the race
// detector can show a second lock creeping back in around it.
func TestNodeInfoControlFramesWriteInSeqOrder(t *testing.T) {
	const (
		senders     = 8
		perSender   = 16
		totalFrames = senders * perSender
	)

	var sink lockedBuffer
	n := newNodeInfoHandle(&sink)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for s := range senders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release every sender at once to maximise overlap
			for i := range perSender {
				// Half the senders take each path, so the two are interleaved
				// rather than running in sequence.
				var err error
				if (s+i)%2 == 0 {
					err = n.SetClusterIdentity("principal-" + strconv.Itoa(s) + "-" + strconv.Itoa(i))
				} else {
					err = n.SetLogLevel("debug")
				}
				if err != nil {
					t.Errorf("send from sender %d: %v", s, err)
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	reader := NewCodec(readWriter{Reader: bytes.NewReader(sink.Bytes()), Writer: io.Discard})
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
		if !msg.IsNotification() {
			t.Fatalf("frame with seq %d is not a notification: %+v", want, msg)
		}
	}
	if _, err := reader.Read(); err != io.EOF {
		t.Fatalf("expected exactly %d frames then EOF, got err %v", totalFrames, err)
	}
}

// TestWorkloadManagerForwardWritesInSeqOrder pins the wire-order contract for the
// broker's busiest child push: a workload event reaches Forward from the
// client's read loop, from rehydration, and from the discovery snapshot, so
// concurrent senders are normal. The seq the manager reads must equal the frame's
// position on the wire — §2.7 gap detection reads an inversion as a dropped frame
// and answers with a spurious resync.
func TestWorkloadManagerForwardWritesInSeqOrder(t *testing.T) {
	const (
		senders     = 8
		perSender   = 16
		totalFrames = senders * perSender
	)

	var sink lockedBuffer
	m := &workloadManagerProcess{codec: NewCodec(writeOnlyReadWriter{&sink})}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for s := range senders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release every sender at once to maximise overlap
			for range perSender {
				params := json.RawMessage(`{"sender":` + strconv.Itoa(s) + `}`)
				if err := m.Forward(noderec.NotifyNodes, params); err != nil {
					t.Errorf("forward: %v", err)
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	reader := NewCodec(readWriter{Reader: bytes.NewReader(sink.Bytes()), Writer: io.Discard})
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

// assertStamped fails unless the single frame in line carries an epoch and a seq,
// and carries nothing that would make jsonrpc.validate() reject it as something
// other than a notification.
func assertStamped(t *testing.T, line string) {
	t.Helper()
	var frame struct {
		Epoch *string `json:"epoch"`
		Seq   *uint64 `json:"seq"`
		ID    *uint64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		t.Fatalf("decode frame %q: %v", line, err)
	}
	if frame.Epoch == nil || *frame.Epoch != jsonrpc.Epoch() {
		t.Errorf("epoch = %v, want %s", frame.Epoch, jsonrpc.Epoch())
	}
	if frame.Seq == nil || *frame.Seq == 0 {
		t.Errorf("seq = %v, want a number from 1", frame.Seq)
	}
	if frame.ID != nil {
		t.Errorf("frame carries an id (%v); a stamped request or response is rejected by validate()", frame.ID)
	}
	// And the canonical reader must accept it, which is the whole point.
	reader := NewCodec(readWriter{Reader: strings.NewReader(line), Writer: io.Discard})
	if _, err := reader.Read(); err != nil {
		t.Errorf("canonical reader rejected the frame: %v", err)
	}
}

// lockedBuffer is safe to hand to a codec several goroutines write through. The
// codec serializes its own writes, but the test does not assume that and the
// buffer is cheap to make safe.
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
