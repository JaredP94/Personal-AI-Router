// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestWorkerExitMidCancelInterruptsDownloadAndAllowsRetry(t *testing.T) {
	var out bytes.Buffer
	b := &Broker{codec: NewCodec(writeOnlyReadWriter{&out}), engineSubscribed: true}
	old := b.beginDownloadInstance()
	w := &rpcWorker{}
	b.setEngineMgr(w)
	request := &Message{Method: "engine:action", Params: json.RawMessage(`{"engine":"ollama","action":"pull_model","opId":"old","params":{"name":"model"}}`)}
	b.trackDownload(w, request)
	// The cancel acceptance does not settle the tracked transfer.
	b.trackDownload(w, &Message{Method: "engine:cancel-pull", Params: json.RawMessage(`{"engine":"ollama","model":"model","opId":"old"}`)})
	b.setEngineMgr(nil)
	if len(b.downloads) != 0 || !strings.Contains(out.String(), `"stage":"interrupted"`) {
		t.Fatalf("worker exit did not publish interruption: %s", out.String())
	}
	if strings.Contains(out.String(), `"stage":"cancelled"`) {
		t.Fatal("claimed cancellation succeeded")
	}
	next := b.beginDownloadInstance()
	replacement := &rpcWorker{}
	b.setEngineMgr(replacement)
	request.Params = json.RawMessage(`{"engine":"ollama","action":"pull_model","opId":"new","params":{"name":"model"}}`)
	b.trackDownload(replacement, request)
	before := out.Len()
	b.forwardDownloadInstance(old, "engine:pull-progress", json.RawMessage(`{"engine":"ollama","model":"model","opId":"old","op":"pull","stage":"interrupted"}`))
	if out.Len() != before || b.downloads["new"].owner != next {
		t.Fatal("old instance changed replacement")
	}
	b.forwardDownloadInstance(next, "engine:pull-progress", json.RawMessage(`{"engine":"ollama","model":"model","opId":"new","op":"pull","stage":"done"}`))
	if len(b.downloads) != 0 {
		t.Fatal("terminal update did not retire retry")
	}
}

func TestWorkerExitInterruptsRemoteDownload(t *testing.T) {
	var out bytes.Buffer
	b := &Broker{codec: NewCodec(writeOnlyReadWriter{&out}), engineSubscribed: true}
	b.beginDownloadInstance()
	w := &rpcWorker{}
	b.setEngineMgr(w)
	b.trackDownload(w, &Message{Method: "engine:remote-pull-model", Params: json.RawMessage(`{"node":"peer","engine":"lmstudio","model":"model","opId":"remote"}`)})
	b.setEngineMgr(nil)
	if !strings.Contains(out.String(), `"method":"engine:remote-progress"`) || !strings.Contains(out.String(), `"stage":"interrupted"`) {
		t.Fatal(out.String())
	}
}
