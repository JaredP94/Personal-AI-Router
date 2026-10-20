// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package ui

import (
	"context"
	"net"
	"nvpair-tui/rpc"
	"testing"
	"time"
)

func TestRecoveryConvergesAllSnapshotConsumers(t *testing.T) {
	cli, srv := net.Pipe()
	defer cli.Close()
	defer srv.Close()
	c := rpc.NewClient(cli, cli)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	go func() {
		codec := rpc.NewCodec(srv)
		for {
			q, err := codec.Read()
			if err != nil {
				return
			}
			var result any = map[string]any{}
			switch q.Method {
			case "cluster:get-node-id":
				result = map[string]string{"nodeUuid": "new", "nodeId": "new-host"}
			case "discovery:get-nodes", "nodes:get-initial":
				result = map[string]any{"nodes": []any{}}
			case "errors:get-initial":
				result = []any{}
			case "workloads:get-initial":
				result = map[string]any{"workloads": []any{}}
			case "engine:get-installed":
				result = map[string]any{"engines": []any{}}
			case "workers:get-status":
				result = map[string]any{"workers": []any{map[string]any{"name": "scanner", "state": "running"}}}
			}
			_ = codec.Respond(q.ID, result)
		}
	}()
	n := newNodesView(c)
	n.SetSize(80, 21)
	n.setNodes([]availableNode{{HostUUID: "stale"}})
	w := newWorkloadsView(c)
	w.SetSize(80, 21)
	w.upsert(workload{ID: "stale", OriginatedFrom: "old"})
	cl := newClusterView(c)
	cl.identity.NodeUUID = "old"
	h := newHealthView(c)
	h.setWorkers([]workerStatus{{Name: "stale-worker", State: "exhausted"}})
	e := newErrorsView(c)
	e.SetSize(80, 21)
	e.Update(NotificationMsg{&rpc.Message{Method: "errors:update", Params: []byte(`[{"id":"stale","message":"old error"}]`)}})
	l := newLoop(New(c, nil, []View{n, w, cl, h, e}))
	l.m.connectionSetup = true
	epoch := "new"
	l.turn(t, NotificationMsg{&rpc.Message{Method: "heartbeat", Epoch: &epoch}})
	deadline := time.After(time.Second)
	for len(n.nodes) != 0 || len(w.byKey) != 0 || cl.identity.NodeUUID != "new" || len(h.workers) != 1 || h.workers[0].Name != "scanner" || len(e.errs) != 0 {
		select {
		case msg := <-l.inbox:
			l.turn(t, msg)
		case <-deadline:
			t.Fatalf("stale state nodes=%v workloads=%v cluster=%v overview=%v", n.nodes, w.byKey, cl.identity, h.workers)
		}
	}
}
