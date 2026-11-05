// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"nvpair-tui/rpc"
	"strings"
	"testing"
)

func downloadParams(t *testing.T, params, id string) string {
	t.Helper()
	var p map[string]json.RawMessage
	if err := json.Unmarshal([]byte(params), &p); err != nil {
		t.Fatal(err)
	}
	p["opId"], _ = json.Marshal(id)
	result, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(result)
}

func TestDownloadCancelAcceptedIsNotStopped(t *testing.T) {
	v := newEnginesView(nil)
	v.downloads = []engineDownload{{id: "one", engine: "ollama", model: "a", canCancel: true, status: "downloading"}}
	v.downloads[0].status = "canceling"
	v.Update(downloadCancelMsg{id: "one", accepted: true})
	v.Update(downloadCancelMsg{id: "one", accepted: false})
	if len(v.downloads) != 1 || v.downloads[0].status != "canceling" {
		t.Fatal("acceptance retired download")
	}
	v.applyDownloadProgress(downloadProgress{OpID: "one", Engine: "ollama", Model: "a", Stage: "downloading", Percent: 60, CanCancel: true})
	if v.downloads[0].status != "canceling" {
		t.Fatal("progress reverted Canceling")
	}
	v.applyDownloadProgress(downloadProgress{OpID: "one", Engine: "ollama", Model: "a", Stage: "cancelled"})
	if len(v.downloads) != 0 || !strings.Contains(v.status, "Partial data is kept") {
		t.Fatalf("confirmation not rendered: %s", v.status)
	}
}

func TestDownloadInterruptedOnEpochWorkerRestartAndPeerLoss(t *testing.T) {
	for _, event := range []struct {
		name    string
		message interface{}
	}{
		{"epoch", epochChangedMsg{}},
		{"worker exit", workersLoadedMsg{workers: []workerStatus{{Name: "engine-manager", State: "restarting"}}}},
		{"worker replaced", workersLoadedMsg{workers: []workerStatus{{Name: "engine-manager", State: "running", PID: 2}}}},
		{"peer lost", discoverySnapshotMsg{}},
		{"peer interrupted", NotificationMsg{Msg: &rpc.Message{Method: "engine:remote-progress", Params: json.RawMessage(`{"node":"peer","engine":"ollama","model":"a","opId":"old","stage":"interrupted"}`)}}},
	} {
		t.Run(event.name, func(t *testing.T) {
			v := newEnginesView(nil)
			v.target = "peer"
			v.downloadWorker = &workerStatus{Name: "engine-manager", State: "running", PID: 1}
			v.downloads = []engineDownload{{id: "old", node: "peer", engine: "ollama", model: "a", status: "canceling"}}
			v.Update(event.message)
			if len(v.downloads) != 0 || v.status != "Download interrupted — retry to resume" {
				t.Fatalf("stuck cancel: %+v %s", v.downloads, v.status)
			}
			v.downloads = []engineDownload{{id: "new", node: "peer", engine: "ollama", model: "a", status: "pulling"}}
			v.applyDownloadProgress(downloadProgress{OpID: "old", Node: "peer", Engine: "ollama", Model: "a", Stage: "interrupted"})
			if len(v.downloads) != 1 || v.downloads[0].id != "new" {
				t.Fatal("old interruption overwrote retry")
			}
		})
	}
}

func TestDownloadOldStopCannotRetireNewPull(t *testing.T) {
	v := newEnginesView(nil)
	v.downloads = []engineDownload{{id: "new", node: "peer", engine: "lmstudio", model: "a", canCancel: true}}
	v.target = "peer"
	v.applyDownloadProgress(downloadProgress{OpID: "old", Node: "peer", Engine: "lmstudio", Model: "a", Stage: "cancelled"})
	v.modelOperationDone(modelOpMsg{node: "peer", engine: "lmstudio", model: "a", action: "pull", opID: "old"})
	if len(v.downloads) != 1 || v.downloads[0].id != "new" {
		t.Fatal("late completion retired replacement")
	}
}
