// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// A cancellation fence joins work; it does not claim to undo a bind that
// finished before cancellation. The port owner must still converge to intent.
func TestProxyCanceledCommittedWriteConvergesToAcceptedPort(t *testing.T) {
	h := newSettingsHarness(t)
	request := h.request(t)
	h.b.engineConfigMu.Lock()
	h.b.engineSettings["ollama"].Explicit = true
	h.b.engineConfigMu.Unlock()
	worker, codec := newTestRPCWorkerPipe(t)
	initial := request.Settings.ProxyPort
	candidate := initial + 1
	proxy := &proxyProcess{peer: worker.peer, ready: true, port: initial}
	h.b.setProxy(proxy)
	entered := make(chan struct{})
	releaseLate := make(chan struct{})
	late := make(chan struct{})
	defer func() {
		select {
		case <-releaseLate:
		default:
			close(releaseLate)
		}
	}()
	go func() {
		var pending *Message
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			switch msg.Method {
			case "set-port":
				if pending == nil {
					pending = msg
					close(entered)
					go func() { <-releaseLate; _ = codec.Respond(msg.ID, map[string]int{"port": candidate}); close(late) }()
					continue
				}
				var p struct{ Port int }
				_ = json.Unmarshal(msg.Params, &p)
				proxy.handleNotify("ready", settingsJSON(map[string]int{"port": p.Port}))
				_ = codec.Respond(msg.ID, map[string]int{"port": p.Port})
			case "internal:cancel-set-port":
				// The old bind committed, and all its ready notifications precede the
				// fence acknowledgement, just as on a real proxy's ordered codec.
				proxy.handleNotify("ready", settingsJSON(map[string]int{"port": candidate}))
				_ = codec.Respond(msg.ID, nil)
			default:
				_ = codec.Respond(msg.ID, nil)
			}
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		finish := h.b.beginPortOperation("ollama")
		_, _, err := writeProxyPort(ctx, proxy, settingsJSON(map[string]int{"port": candidate}))
		finish()
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("write not entered")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled write succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled write did not converge")
	}
	if ready, port := proxy.Status(); !ready || port != initial {
		t.Fatalf("fenced committed write left port=%d ready=%v", port, ready)
	}
	close(releaseLate)
	select {
	case <-late:
	case <-time.After(time.Second):
		t.Fatal("late response not sent")
	}
	if _, rpcErr, err := proxy.Call(context.Background(), "worker/health", nil); rpcErr != nil || err != nil {
		t.Fatalf("late-response barrier: %v %v", rpcErr, err)
	}
	if _, port := proxy.Status(); port != initial {
		t.Fatalf("late response replaced accepted port with %d", port)
	}
}
