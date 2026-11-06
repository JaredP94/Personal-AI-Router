// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"
)

type pairingPublicationWriter struct {
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	frames  []Message
}

func (w *pairingPublicationWriter) Read([]byte) (int, error) { return 0, io.EOF }
func (w *pairingPublicationWriter) Write(p []byte) (int, error) {
	var msg Message
	if err := json.Unmarshal(p, &msg); err != nil {
		return 0, err
	}
	if msg.Method == "cluster:pairing-completed" {
		w.entered <- struct{}{}
		<-w.release
	}
	w.mu.Lock()
	w.frames = append(w.frames, msg)
	w.mu.Unlock()
	return len(p), nil
}

// The real completion and removal execute concurrently. The publication seam
// stalls the ordered stream, and channels order the interleaving: no sleeps
// or assumption that the broker happened to receive the completion first.
func TestPairingCompletionPublicationOrdersConcurrentRemoval(t *testing.T) {
	m := newTestManager(t)
	peer := newPinFixture(t, "peer")
	pi, cert := pairingInfoFor(t, peer)
	pi.Addr = "" // no network removal leg is needed for the composition transaction
	m.putInvite(&Invite{InviteID: "ordered", ClusterID: "cluster-1", State: inviteStatePending})
	sess := putInviterSession(t, m, "ordered", "cluster-1")
	sess.joinerAddr = "overlay.example:14321"
	writer := &pairingPublicationWriter{entered: make(chan struct{}, 1), release: make(chan struct{})}
	m.codec = NewCodec(writer)
	var once sync.Once
	unblock := func() { once.Do(func() { close(writer.release) }) }
	t.Cleanup(unblock)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	completed := make(chan error, 1)
	go func() { completed <- m.finalizePairing("ordered", sess, pi, cert) }()
	select {
	case <-writer.entered:
	case <-ctx.Done():
		t.Fatal("completion never reached publication")
	}
	started, removed := make(chan struct{}), make(chan struct{})
	go func() {
		close(started)
		id := json.RawMessage("99")
		params, _ := json.Marshal(map[string]string{"nodeUuid": peer.uuid})
		m.handleNodesRemove(&Message{JSONRPC: "2.0", ID: &id, Method: "nodes:remove", Params: params})
		close(removed)
	}()
	<-started
	unblock()
	select {
	case err := <-completed:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("completion stalled")
	}
	select {
	case <-removed:
	case <-ctx.Done():
		t.Fatal("removal stalled")
	}
	if err := m.codec.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	completionIndex, removalIndex := -1, -1
	for i, msg := range writer.frames {
		if msg.Method == "cluster:pairing-completed" {
			completionIndex = i
		}
		if msg.ID != nil && string(*msg.ID) == "99" {
			var result struct {
				Removed bool `json:"removed"`
			}
			_ = json.Unmarshal(msg.Result, &result)
			if msg.Error != nil || !result.Removed {
				t.Fatalf("real removal failed: %+v", msg)
			}
			removalIndex = i
		}
	}
	if completionIndex < 0 || removalIndex <= completionIndex {
		t.Fatalf("completion/removal publication order: %d, %d", completionIndex, removalIndex)
	}
}
