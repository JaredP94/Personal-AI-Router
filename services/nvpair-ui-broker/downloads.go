// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"nvpair-shared/jsonrpc"
)

// The broker owns the lifetime of relayed downloads. Each worker launch has a
// separate identity, including launches within the same broker protocol epoch.
// No lock is held while publishing notifications or calling a worker.
type downloadInstance struct {
	id     string
	worker *rpcWorker
}
type brokerDownload struct {
	owner      *downloadInstance
	OpID       string `json:"opId"`
	Node       string `json:"node,omitempty"`
	Engine     string `json:"engine"`
	Model      string `json:"model"`
	Op         string `json:"op"`
	Stage      string `json:"stage"`
	Message    string `json:"message,omitempty"`
	InstanceID string `json:"instanceId"`
}

func (b *Broker) beginDownloadInstance() *downloadInstance {
	b.interruptDownloads()
	b.downloadMu.Lock()
	defer b.downloadMu.Unlock()
	b.downloadSerial++
	instance := &downloadInstance{id: fmt.Sprintf("%s/%d", jsonrpc.Epoch(), b.downloadSerial)}
	b.downloadInstance = instance
	return instance
}

func (b *Broker) trackDownload(w *rpcWorker, msg *Message) {
	var p struct {
		OpID, Node, Engine, Model, Action string
		Params                            struct{ Name, Model string }
	}
	if json.Unmarshal(msg.Params, &p) != nil || p.OpID == "" ||
		!(msg.Method == "engine:remote-pull-model" || msg.Method == "engine:action" && p.Action == "pull_model") {
		return
	}
	if p.Params.Name != "" {
		p.Model = p.Params.Name
	} else if p.Params.Model != "" {
		p.Model = p.Params.Model
	}
	b.downloadMu.Lock()
	defer b.downloadMu.Unlock()
	owner := b.downloadInstance
	if owner == nil || owner.worker != w {
		return
	}
	if b.downloads == nil {
		b.downloads = make(map[string]brokerDownload)
	}
	b.downloads[p.OpID] = brokerDownload{owner: owner, OpID: p.OpID, Node: p.Node, Engine: p.Engine, Model: p.Model, Op: "pull", Stage: "pulling", InstanceID: owner.id}
}

// A transport exit can win the supervisor callback. Publish interruption before
// returning the failed call, rather than letting a Canceling row await a stop.
func (b *Broker) settleDownload(w *rpcWorker, msg *Message, callErr error, rpcErr *RPCError) {
	var input struct {
		OpID string `json:"opId"`
	}
	if json.Unmarshal(msg.Params, &input) != nil || input.OpID == "" {
		return
	}
	if msg.Method != "engine:action" && msg.Method != "engine:remote-pull-model" {
		return
	}
	b.downloadMu.Lock()
	p, ok := b.downloads[input.OpID]
	if ok && p.owner.worker == w {
		delete(b.downloads, input.OpID)
	} else {
		ok = false
	}
	b.downloadMu.Unlock()
	if !ok {
		return
	}
	p.Stage = "done"
	if callErr != nil {
		p.Stage, p.Message = "interrupted", "Download interrupted — retry to resume"
	} else if rpcErr != nil {
		p.Stage = "error"
	}
	method := "engine:pull-progress"
	if p.Node != "" {
		method = "engine:remote-progress"
	}
	params, _ := json.Marshal(p)
	b.forwardEngineNotification(method, params)
}

func (b *Broker) forwardDownloadInstance(owner *downloadInstance, method string, params json.RawMessage) {
	b.downloadMu.Lock()
	current := b.downloadInstance == owner
	if current && (method == "engine:pull-progress" || method == "engine:remote-progress") {
		var p brokerDownload
		if json.Unmarshal(params, &p) == nil && p.OpID != "" && (p.Op == "pull" || p.Op == "pull_model") {
			if active, ok := b.downloads[p.OpID]; ok && active.owner == owner && downloadTerminal(p.Stage) {
				delete(b.downloads, p.OpID)
			}
		}
	}
	b.downloadMu.Unlock()
	if current {
		b.forwardEngineNotification(method, params)
	}
}

func downloadTerminal(stage string) bool {
	return stage == "done" || stage == "error" || stage == "cancelled" || stage == "interrupted"
}

func (b *Broker) interruptDownloads() {
	b.downloadMu.Lock()
	interrupted := make([]brokerDownload, 0, len(b.downloads))
	for _, p := range b.downloads {
		interrupted = append(interrupted, p)
	}
	b.downloads = nil
	b.downloadInstance = nil
	b.downloadMu.Unlock()
	for _, p := range interrupted {
		p.Stage, p.Message = "interrupted", "Download interrupted — retry to resume"
		method := "engine:pull-progress"
		if p.Node != "" {
			method = "engine:remote-progress"
		}
		params, _ := json.Marshal(p)
		b.forwardEngineNotification(method, params)
	}
}
