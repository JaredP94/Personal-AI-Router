// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	settings "nvpair-shared/enginesettings"
)

func TestLoadProfileRelayUsesCapturedWorkerWithoutSettingsLocks(t *testing.T) {
	worker, codec := newTestRPCWorkerPipe(t)
	b := &Broker{codec: NewCodec(&bytes.Buffer{}), nodeID: "local"}
	b.setEngineMgr(worker)
	seen := make(chan string, 2)
	go func() {
		for i := 0; i < 2; i++ {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			b.engineConfigMu.Lock()
			b.settingsApplyMu.Lock()
			b.settingsApplyMu.Unlock()
			b.engineConfigMu.Unlock()
			seen <- msg.Method
			_ = codec.Respond(msg.ID, settings.LoadProfileSnapshot{Engine: "lmstudio", Model: "m", LoadText: "--gpu max", Format: "pair-arguments-v1", Editable: true})
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, node := range []string{"local", "peer"} {
		result, err := b.relayLoadProfile(ctx, "get-load-profile", settings.Request{NodeID: node, Engine: "lmstudio", Model: "m"})
		if err != nil {
			t.Fatalf("relay deadlocked or failed: %v", err)
		}
		var snap settings.LoadProfileSnapshot
		if json.Unmarshal(result, &snap) != nil || snap.NodeID != node || snap.LoadText != "--gpu max" {
			t.Fatalf("incorrect snapshot: %s", result)
		}
	}
	if <-seen != "engine:get-load-profile" || <-seen != "engine:remote-get-load-profile" {
		t.Fatal("incorrect local/verified-peer routing")
	}
}

func TestLoadProfileRelayFencesReplacementWorkerCompletion(t *testing.T) {
	worker, codec := newTestRPCWorkerPipe(t)
	replacement, _ := newTestRPCWorkerPipe(t)
	b := &Broker{nodeID: "local"}
	b.setEngineMgr(worker)
	go func() {
		msg, err := codec.Read()
		if err != nil {
			return
		}
		b.setEngineMgr(replacement)
		_ = codec.Respond(msg.ID, settings.LoadProfileSnapshot{Engine: "lmstudio", Model: "m", Format: "pair-arguments-v1", Editable: true})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := b.relayLoadProfile(ctx, "get-load-profile", settings.Request{Engine: "lmstudio", Model: "m"}); err == nil {
		t.Fatal("superseded worker published a late snapshot")
	}
}

func TestLoadProfileRelayRejectsOtherEnginesAndCancellation(t *testing.T) {
	b := &Broker{nodeID: "local"}
	if _, err := b.relayLoadProfile(context.Background(), "apply-load-profile", settings.Request{Engine: "ollama", Model: "m"}); err == nil {
		t.Fatal("non-LM Studio edit accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.relayLoadProfile(ctx, "get-load-profile", settings.Request{Engine: "lmstudio", Model: "m"}); err == nil {
		t.Fatal("canceled relay accepted")
	}
}
