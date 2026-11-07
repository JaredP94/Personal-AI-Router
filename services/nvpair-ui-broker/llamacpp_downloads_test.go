// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestLlamaDownloadWorkerRestartInterruptsAndFencesLateEvents(t *testing.T) {
	var out bytes.Buffer
	b := &Broker{codec: NewCodec(writeOnlyReadWriter{&out}), engineSubscribed: true}
	old := b.beginDownloadInstance()
	worker := &rpcWorker{}
	b.setEngineMgr(worker)
	b.trackDownload(worker, &Message{Method: "engine:action", Params: json.RawMessage(`{"engine":"llamacpp","action":"pull_model","opId":"old","params":{"model":"a/b:Q4_K_M"}}`)})
	b.setEngineMgr(nil)
	if !strings.Contains(out.String(), `"engine":"llamacpp"`) || !strings.Contains(out.String(), `"stage":"interrupted"`) {
		t.Fatalf("no attributed interruption: %s", out.String())
	}
	next := b.beginDownloadInstance()
	replacement := &rpcWorker{}
	b.setEngineMgr(replacement)
	b.trackDownload(replacement, &Message{Method: "engine:action", Params: json.RawMessage(`{"engine":"llamacpp","action":"pull_model","opId":"new","params":{"model":"a/b:Q4_K_M"}}`)})
	before := out.Len()
	b.forwardDownloadInstance(old, "engine:pull-progress", json.RawMessage(`{"engine":"llamacpp","model":"a/b:Q4_K_M","opId":"old","op":"pull","stage":"done"}`))
	if out.Len() != before || b.downloads["new"].owner != next {
		t.Fatal("late old-worker event changed the replacement operation")
	}
	b.forwardDownloadInstance(next, "engine:pull-progress", json.RawMessage(`{"engine":"llamacpp","model":"a/b:Q4_K_M","opId":"new","op":"pull","stage":"done"}`))
	if len(b.downloads) != 0 {
		t.Fatal("replacement did not finish")
	}
}
