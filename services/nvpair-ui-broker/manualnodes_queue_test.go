// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A manual-nodes worker that never answers must not stall the broker's request
// loop: a second mutation queues behind the first instead of blocking the
// caller, and mutations still apply in order once the worker answers.
func TestManualNodeMutationsDoNotBlockTheRequestLoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "configs", "manual-nodes.json")
	clientBroker, clientSide := net.Pipe()
	workerBroker, workerSide := net.Pipe()
	t.Cleanup(func() {
		clientBroker.Close()
		clientSide.Close()
		workerBroker.Close()
		workerSide.Close()
	})
	worker := &rpcWorker{peer: NewPeer(NewCodec(workerBroker))}
	go worker.peer.Serve(nil, nil)

	// The worker reads requests but answers only when released.
	release := make(chan struct{})
	received := make(chan string, 4)
	go func() {
		codec := NewCodec(workerSide)
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			received <- msg.Method
			<-release
			_ = codec.Respond(msg.ID, map[string]any{"id": "manual:node.local"})
		}
	}()
	responses := make(chan *Message, 4)
	go func() {
		codec := NewCodec(clientSide)
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			responses <- msg
		}
	}()

	b := &Broker{codec: NewCodec(clientBroker), manualNodesConfigPath: path}
	b.setManualNodes(worker)
	send := func(id, method, params string) {
		raw := json.RawMessage(id)
		b.relayToManualNodes(&Message{JSONRPC: "2.0", ID: &raw, Method: method, Params: json.RawMessage(params)})
	}

	returned := make(chan struct{})
	go func() {
		send("1", "node/add", `{"address":"node.local"}`)
		send("2", "node/remove", `{"id":"manual:node.local"}`)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("a second mutation blocked the caller while the first was unanswered")
	}
	if got := <-received; got != "node/add" {
		t.Fatalf("first request = %s, want node/add", got)
	}
	select {
	case got := <-received:
		t.Fatalf("%s reached the worker before node/add was answered", got)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	if got := <-received; got != "node/remove" {
		t.Fatalf("second request = %s, want node/remove", got)
	}
	for _, want := range []string{"1", "2"} {
		select {
		case r := <-responses:
			if string(*r.ID) != want || r.Error != nil {
				t.Fatalf("response = %s %+v, want id %s success", *r.ID, r.Error, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no response for request %s", want)
		}
	}
	entries, err := loadPersistedManualNodes(path)
	if err != nil || len(entries) != 0 {
		t.Fatalf("persisted = %+v, %v; want the add then the remove applied", entries, err)
	}
}

// The file the desktop app used to own carries {id, address, name}; the broker
// reads it as is, so existing manual nodes survive the move.
func TestManualNodesFileFromTheDesktopStoreLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manual-nodes.json")
	legacy := `[{"id":"gpu-box","address":"100.101.102.103","name":"gpu-box"}]`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := loadPersistedManualNodes(path)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	if e := entries[0]; e.ID != "gpu-box" || e.Address != "100.101.102.103" || e.Name != "gpu-box" || e.key() != "gpu-box" {
		t.Fatalf("entry = %+v", e)
	}
}
