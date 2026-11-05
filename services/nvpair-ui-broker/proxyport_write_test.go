// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	settings "nvpair-shared/enginesettings"
)

// Removing the proxy write deadline leaves the first Apply (and every other
// engine's Apply behind settingsApplyMu) stuck until the suspended reply arrives.
func TestProxyWriteHangReleasesApplyAndFencesLateReply(t *testing.T) {
	h := newSettingsHarness(t)
	request := h.request(t)
	request.Settings.LaunchText += " --hung-proxy"
	worker, codec := newTestRPCWorkerPipe(t)
	_, initial := h.b.getProxy().Status()
	proxy := &proxyProcess{peer: worker.peer, ready: true, port: initial}
	h.b.setProxy(proxy)
	entered, release, late := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	go func() {
		var pending *Message
		var operationID string
		canceled := false
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			if msg.Method == "set-port" && pending == nil {
				pending = msg
				var params struct{ OperationID string }
				_ = json.Unmarshal(msg.Params, &params)
				operationID = params.OperationID
				close(entered)
				go func() {
					<-release
					// A late successful reply must not update broker state after
					// the canceled write has been fenced and Apply has returned.
					_ = codec.Respond(msg.ID, map[string]int{"port": 11434})
					close(late)
				}()
				continue
			}
			if msg.Method == "internal:cancel-set-port" {
				var params struct{ OperationID string }
				_ = json.Unmarshal(msg.Params, &params)
				if operationID == "" || params.OperationID != operationID {
					_ = codec.RespondError(msg.ID, -32000, "wrong operation")
					continue
				}
				canceled = true
				_ = codec.Respond(msg.ID, nil) // the worker has fenced the write
				continue
			}
			if msg.Method == "set-port" && !canceled {
				_ = codec.RespondError(msg.ID, -32000, "write was not fenced")
				continue
			}
			_ = codec.Respond(msg.ID, map[string]int{"port": initial})
		}
	}()
	type outcome struct {
		receipt settingsReceipt
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		receipt, err := h.b.applyEngineSettings(context.Background(), request, "")
		done <- outcome{receipt, err}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("proxy write not entered")
	}
	select {
	case result := <-done:
		if result.err != nil || result.receipt.Phase != "failed" {
			t.Fatalf("hung Apply did not fail safely: %+v", result)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("hung proxy write retained settingsApplyMu beyond the write bound")
	}
	other := settings.Request{Engine: "lmstudio", RequestID: settingsID()}
	snapshot, err := h.b.getEngineSettings(context.Background(), other, "")
	if err != nil {
		t.Fatal(err)
	}
	other.Settings, other.ExpectedRevision = snapshot.Settings, snapshot.Revision
	other.Settings.LaunchText += " --unrelated"
	go func() {
		receipt, err := h.b.applyEngineSettings(context.Background(), other, "")
		done <- outcome{receipt, err}
	}()
	select {
	case result := <-done:
		if result.err != nil || result.receipt.Phase != "succeeded" {
			t.Fatalf("unrelated Apply: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("unrelated Apply blocked behind hung proxy")
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-late:
	case <-time.After(time.Second):
		t.Fatal("late reply not delivered")
	}
	// A successful following round trip orders the peer reader after the late
	// reply; this checks the actual broker state, not just the fake's cancel flag.
	if _, rpcErr, err := proxy.Call(context.Background(), "worker/health", nil); err != nil || rpcErr != nil {
		t.Fatalf("proxy fence barrier: %v %v", rpcErr, err)
	}
	if _, actual := proxy.Status(); actual != initial {
		t.Fatalf("late reply applied port %d; accepted previous port is %d", actual, initial)
	}
}

func TestProxyPortWriteHonorsCancellation(t *testing.T) {
	h := newSettingsHarness(t)
	worker, codec := newTestRPCWorkerPipe(t)
	proxy := &proxyProcess{peer: worker.peer, ready: true, port: 11435}
	h.b.setProxy(proxy)
	entered := make(chan struct{})
	go func() {
		_, err := codec.Read()
		if err != nil {
			return
		}
		close(entered)
		cancel, err := codec.Read()
		if err == nil && cancel.Method == "internal:cancel-set-port" {
			_ = codec.Respond(cancel.ID, nil)
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.b.rebindSettingsProxy(ctx, "ollama", 11436) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("proxy write not entered")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled proxy write succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("proxy write ignored caller cancellation")
	}
}
