// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"
)

// A worker that never drains stdin. Stop severs the same pipe the relay uses.
type stalledRelayWorker struct {
	*fakeHandle
	writer *io.PipeWriter
	peer   *Peer
}

func (h *stalledRelayWorker) Stop() {
	_ = h.writer.Close()
	h.peer.Close()
	h.fakeHandle.Stop()
}

type observedRelayWriter struct {
	io.Writer
	entered  chan struct{}
	finished chan struct{}
	once     sync.Once
}

func (w *observedRelayWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	n, err := w.Writer.Write(p)
	close(w.finished)
	return n, err
}

type relayResponseWriter struct{ messages chan Message }

func (w relayResponseWriter) Write(p []byte) (int, error) {
	var msg Message
	if err := json.Unmarshal(p, &msg); err != nil {
		return 0, err
	}
	w.messages <- msg
	return len(p), nil
}

func TestWorkersRestartWhileEngineRelayWriteBlocked(t *testing.T) {
	responses := make(chan Message, 32)
	b := NewBroker(NewCodec(writeOnlyReadWriter{relayResponseWriter{responses}}), workerPaths{engineMgr: "fixture"})
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	observed := &observedRelayWriter{Writer: writer, entered: make(chan struct{}), finished: make(chan struct{})}
	peer := NewPeer(NewCodec(writeOnlyReadWriter{observed}))
	defer peer.Close()
	b.setEngineMgr(&rpcWorker{peer: peer})
	h := &stalledRelayWorker{fakeHandle: newFakeHandle(), writer: writer, peer: peer}
	entry := b.workerEntries[3]
	entry.sup.spawn = func() (supervisedHandle, error) { return h, nil }
	entry.sup.StartRetrying()
	entry.started = true
	defer entry.sup.Stop()

	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		relayID, restartID := json.RawMessage(`101`), json.RawMessage(`102`)
		// Match the broker's serial client dispatch, not independent goroutines.
		b.handleMessage(&Message{JSONRPC: "2.0", ID: &relayID, Method: "engine:pull-model"})
		<-observed.entered
		b.handleMessage(&Message{JSONRPC: "2.0", ID: &restartID, Method: "workers:restart", Params: json.RawMessage(`{"name":"engine-manager"}`)})
	}()
	select {
	case <-observed.entered:
	case <-time.After(time.Second):
		t.Fatal("relay write never entered")
	}
	select {
	case <-loopDone:
	case <-time.After(time.Second):
		t.Fatal("blocked engine relay prevented workers:restart on the client loop")
	}
	gotRelay, gotRestart := false, false
	deadline := time.After(time.Second)
	for !gotRelay || !gotRestart {
		select {
		case msg := <-responses:
			if msg.ID == nil {
				continue
			}
			switch string(*msg.ID) {
			case "101":
				if msg.Error == nil {
					t.Fatalf("blocked relay did not fail: %+v", msg)
				}
				gotRelay = true
			case "102":
				if msg.Error != nil {
					t.Fatalf("restart rejected: %+v", msg)
				}
				gotRestart = true
			}
		case <-deadline:
			t.Fatal("restart did not fail the blocked relay and answer the client")
		}
	}
	select {
	case <-observed.finished:
	case <-time.After(time.Second):
		t.Fatal("restart leaked blocked writer")
	}
}
