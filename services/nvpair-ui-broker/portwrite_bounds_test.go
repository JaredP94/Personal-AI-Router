// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	settings "nvpair-shared/enginesettings"
)

func TestProxyPortUnacknowledgedCancellationRetiresWorker(t *testing.T) {
	worker, codec := newTestRPCWorkerPipe(t)
	proxy := &proxyProcess{peer: worker.peer, ready: true, port: 11435}
	entered := make(chan struct{})
	canceled := make(chan string, 1)
	go func() {
		msg, err := codec.Read()
		if err != nil {
			return
		}
		var request struct {
			OperationID string `json:"operationId"`
		}
		_ = json.Unmarshal(msg.Params, &request)
		close(entered)
		msg, err = codec.Read()
		if err != nil {
			return
		}
		var cancel struct {
			OperationID string `json:"operationId"`
		}
		_ = json.Unmarshal(msg.Params, &cancel)
		if msg.Method == "internal:cancel-set-port" && request.OperationID != "" && cancel.OperationID == request.OperationID {
			canceled <- request.OperationID
		}
		// Never acknowledge cancellation: the captured peer must be retired.
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := writeProxyPort(ctx, proxy, settingsJSON(map[string]int{"port": 11436}))
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
			t.Fatal("unfenced write succeeded")
		}
	case <-time.After(portFenceTimeout + time.Second):
		t.Fatal("cancellation join exceeded its bound")
	}
	select {
	case <-canceled:
	default:
		t.Fatal("write was not canceled by operation ID")
	}
	proxy.handleNotify("ready", settingsJSON(map[string]int{"port": 11436}))
	if ready, port := proxy.Status(); ready || port != 11435 {
		t.Fatalf("retired worker revived: ready=%v port=%d", ready, port)
	}
	if _, _, err := writeProxyPort(context.Background(), proxy, settingsJSON(map[string]int{"port": 11435})); err == nil {
		t.Fatal("retired worker accepted a following write")
	}
}

func TestManagedMoveCancellationAcknowledgementIsBounded(t *testing.T) {
	for _, engine := range []string{"ollama", "lmstudio"} {
		t.Run(engine, func(t *testing.T) {
			worker, codec := newTestRPCWorkerPipe(t)
			b := &Broker{}
			b.setEngineMgr(worker)
			entered := make(chan struct{})
			cancelSeen := make(chan struct{})
			go func() {
				msg, err := codec.Read()
				if err != nil {
					return
				}
				var request struct {
					OperationID string `json:"operationId"`
				}
				_ = json.Unmarshal(msg.Params, &request)
				close(entered)
				msg, err = codec.Read()
				if err != nil {
					return
				}
				var cancel struct {
					OperationID string `json:"operationId"`
				}
				_ = json.Unmarshal(msg.Params, &cancel)
				if msg.Method == "internal:cancel-set-port" && request.OperationID != "" && cancel.OperationID == request.OperationID {
					close(cancelSeen)
				}
			}()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan bool, 1)
			b.engineConfigMu.Lock()
			expected := b.ollamaPortReconcileStateLocked()
			b.engineConfigMu.Unlock()
			go func() {
				if engine == "ollama" {
					_, _, fenced := b.moveManagedOllamaPort(ctx, worker, 11436, expected)
					done <- fenced
				} else {
					_, _, _, fenced := b.moveManagedLMStudioPort(ctx, worker, 1235)
					done <- fenced
				}
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("move not entered")
			}
			cancel()
			select {
			case fenced := <-done:
				if fenced {
					t.Fatal("unacknowledged cancellation reported fenced")
				}
			case <-time.After(portFenceTimeout + time.Second):
				t.Fatal("managed cancellation join is unbounded")
			}
			select {
			case <-cancelSeen:
			default:
				t.Fatal("missing operation-ID cancellation")
			}
			select {
			case <-worker.peer.Done():
			default:
				t.Fatal("unfenced manager remains usable")
			}
		})
	}
}

// A configure reply is not proof that borrowed reverse callbacks have joined.
// Simulate an admitted callback stalled outside worker I/O, then verify Apply
// releases its global mutex and quarantines that operation's captured proxy.
func TestSettingsCallbackJoinIsBounded(t *testing.T) {
	h := newSettingsHarness(t)
	request := h.request(t)
	request.Settings.LaunchText += " --join-bound"
	h.entered, h.release = make(chan struct{}, 1), make(chan struct{})
	done := make(chan settingsReceipt, 1)
	go func() { receipt, _ := h.b.applyEngineSettings(context.Background(), request, ""); done <- receipt }()
	select {
	case <-h.entered:
	case <-time.After(time.Second):
		t.Fatal("configure not entered")
	}
	h.b.settingsRelayMu.Lock()
	var operation activeSettingsOperation
	for _, active := range h.b.activeSettings {
		operation = active
	}
	if operation.Callbacks == nil {
		h.b.settingsRelayMu.Unlock()
		t.Fatal("no active callback tracker")
	}
	operation.Callbacks.Add(1)
	h.b.settingsRelayMu.Unlock()
	defer operation.Callbacks.Done()
	close(h.release)
	select {
	case receipt := <-done:
		if receipt.Phase != "failed" {
			t.Fatalf("unjoined callback accepted: %+v", receipt)
		}
	case <-time.After(proxyCallTimeout + portFenceTimeout + time.Second):
		t.Fatal("Apply callback join is unbounded")
	}
	if !h.b.settingsApplyMu.TryLock() {
		t.Fatal("callback join retained settingsApplyMu")
	}
	h.b.settingsApplyMu.Unlock()
	if operation.Context.Err() == nil {
		t.Fatal("parent did not cancel admitted callbacks")
	}
	if ready, _ := operation.Proxy.Status(); ready {
		t.Fatal("unjoined callback proxy not quarantined")
	}
}

func TestSettingsCallbackCancellationJoinsBeforeApplyReturns(t *testing.T) {
	h := newSettingsHarness(t)
	request := h.request(t)
	worker, codec := newTestRPCWorkerPipe(t)
	h.b.setEngineMgr(worker)
	proxyWorker, proxyCodec := newTestRPCWorkerPipe(t)
	_, initial := h.b.getProxy().Status()
	proxy := &proxyProcess{peer: proxyWorker.peer, ready: true, port: initial}
	h.b.setProxy(proxy)
	entered := make(chan struct{})
	fenced := make(chan struct{})
	go func() {
		for {
			msg, err := proxyCodec.Read()
			if err != nil {
				return
			}
			if msg.Method == "set-port" {
				close(entered)
				continue
			}
			if msg.Method == "internal:cancel-set-port" {
				close(fenced)
			}
			_ = proxyCodec.Respond(msg.ID, nil)
		}
	}()
	go func() {
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			if msg.Method == "engine:configure-launch" {
				var configure settings.Configure
				_ = json.Unmarshal(msg.Params, &configure)
				h.b.handleEngineSettingsRead(settingsJSON(settings.Relay{ID: "borrowed", Method: "rebind", Request: settings.Request{Engine: "ollama", RequestID: configure.OperationID, Settings: configure.Settings}}))
				<-entered
				_ = codec.RespondError(msg.ID, -32000, "configure failed")
			} else if msg.IsRequest() {
				_ = codec.Respond(msg.ID, settings.LaunchState{})
			}
		}
	}()
	done := make(chan error, 1)
	go func() {
		h.b.engineConfigMu.Lock()
		record := h.b.engineSettings["ollama"]
		record.Previous = request.Settings
		err := h.b.runSettingsOperationLocked(context.Background(), "ollama", record)
		h.b.engineConfigMu.Unlock()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("configure failure lost")
		}
	case <-time.After(time.Second):
		t.Fatal("parent did not cancel and join borrowed proxy write")
	}
	select {
	case <-fenced:
	default:
		t.Fatal("Apply returned before proxy cancellation fence")
	}
}
