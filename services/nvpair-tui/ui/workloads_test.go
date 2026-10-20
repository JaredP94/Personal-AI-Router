// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"nvpair-tui/rpc"
)

// workloadSession drives the real RPC client and root recovery loop with wire
// fixtures, so snapshot decoding cannot silently drop scheduledOn.
func workloadSession(t *testing.T, snapshots map[string]json.RawMessage) (*recoveryBroker, *loop, *workloadsView) {
	t.Helper()
	cli, srv := net.Pipe()
	client := rpc.NewClient(cli, cli)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); cli.Close(); srv.Close() })
	go client.Run(ctx)
	b := &recoveryBroker{conn: srv, codec: rpc.NewCodec(srv)}
	go func() {
		for {
			q, err := b.codec.Read()
			if err != nil {
				return
			}
			result := snapshots[q.Method]
			if result == nil {
				result = json.RawMessage(`{}`)
			}
			b.writes.Lock()
			err = b.codec.Respond(q.ID, result)
			b.writes.Unlock()
			if err != nil {
				return
			}
		}
	}()
	w := newWorkloadsView(client)
	l := newLoop(New(client, nil, []View{newNodesView(client), w, newClusterView(client)}))
	l.exec(l.m.setupConnection())
	l.until(t, func() bool { return l.m.connectionSetup })
	b.push(t, "A", 1, "heartbeat", nil)
	l.until(t, func() bool { return l.m.hydratedEpoch == "A" })
	return b, l, w
}

func assertWorkloadDestination(t *testing.T, w *workloadsView, want string) {
	t.Helper()
	columns := w.table.Columns()
	destination := -1
	for i, column := range columns {
		if column.Title == "DESTINATION" {
			destination = i
		}
	}
	if destination < 1 || columns[destination-1].Title != "ORIGIN" {
		t.Fatalf("want DESTINATION immediately after ORIGIN, got %v", columns)
	}
	rows := w.table.Rows()
	if len(rows) != 1 || rows[0][destination] != want {
		t.Fatalf("want destination %q, rows=%v", want, rows)
	}
	if rows[0][destination-1] != "origin-u" {
		t.Fatalf("origin must remain separate from destination, rows=%v", rows)
	}
	if !strings.Contains(w.View(), "DESTINATION") || !strings.Contains(w.View(), want) {
		t.Fatalf("destination %q or its header not visible at 80 columns: %s", want, w.View())
	}
}

func TestWorkloadDestinationSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name, scheduled, want        string
		discovery, members, identity json.RawMessage
	}{
		{name: "discovery name", scheduled: `,"scheduledOn":"aaaaaaaa-1111-2222-3333-444444444444"`, want: "GPU-box", discovery: json.RawMessage(`{"nodes":[{"id":"service-id","hostUuid":"aaaaaaaa-1111-2222-3333-444444444444","name":"GPU-box"}]}`)},
		{name: "cluster member name", scheduled: `,"scheduledOn":"bbbbbbbb-1111-2222-3333-444444444444"`, want: "Member", members: json.RawMessage(`{"nodes":[{"id":"member-id","nodeUuid":"bbbbbbbb-1111-2222-3333-444444444444","name":"Member"}]}`)},
		{name: "local identity name", scheduled: `,"scheduledOn":"cccccccc-1111-2222-3333-444444444444"`, want: "Local", identity: json.RawMessage(`{"nodeUuid":"cccccccc-1111-2222-3333-444444444444","nodeId":"local-id","name":"Local"}`)},
		{name: "unknown UUID", scheduled: `,"scheduledOn":"dddddddd-1111-2222-3333-444444444444"`, want: "dddddddd"},
		{name: "unnamed node", scheduled: `,"scheduledOn":"eeeeeeee-1111-2222-3333-444444444444"`, want: "eeeeeeee", discovery: json.RawMessage(`{"nodes":[{"hostUuid":"eeeeeeee-1111-2222-3333-444444444444","name":""}]}`)},
		{name: "absent", want: "—"},
		{name: "short identifier", scheduled: `,"scheduledOn":"short"`, want: "short"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, w := workloadSession(t, map[string]json.RawMessage{
				"workloads:get-initial": json.RawMessage(`{"workloads":[{"id":"request","model":"model","engine":"ollama","state":"running","originatedFrom":"origin-uuid"` + tc.scheduled + `}]}`),
				"discovery:get-nodes":   tc.discovery,
				"nodes:get-initial":     tc.members,
				"cluster:get-node-id":   tc.identity,
			})
			assertWorkloadDestination(t, w, tc.want)
		})
	}
}

func TestWorkloadDestinationFailoverUpsert(t *testing.T) {
	b, l, w := workloadSession(t, map[string]json.RawMessage{
		"workloads:get-initial": json.RawMessage(`{"workloads":[{"id":"request","originatedFrom":"origin-uuid","scheduledOn":"aaaaaaaa-1111"}]}`),
		"discovery:get-nodes":   json.RawMessage(`{"nodes":[{"hostUuid":"aaaaaaaa-1111","name":"First"},{"hostUuid":"bbbbbbbb-2222","name":"Second"}]}`),
	})
	assertWorkloadDestination(t, w, "First")
	b.push(t, "A", 2, "workloads:upsert", json.RawMessage(`{"workloadInfo":{"id":"request","originatedFrom":"origin-uuid","scheduledOn":"bbbbbbbb-2222","state":"running"}}`))
	l.settle(t)
	assertWorkloadDestination(t, w, "Second")
	// A later complete upsert without a destination must clear the old one.
	b.push(t, "A", 3, "workloads:upsert", json.RawMessage(`{"workloadInfo":{"id":"request","originatedFrom":"origin-uuid","state":"pending"}}`))
	l.settle(t)
	assertWorkloadDestination(t, w, "—")
}

func TestWorkloadDestinationTracksDiscoveryWithoutLosingUUID(t *testing.T) {
	b, l, w := workloadSession(t, map[string]json.RawMessage{
		"workloads:get-initial": json.RawMessage(`{"workloads":[{"id":"request","originatedFrom":"origin-uuid","scheduledOn":"aaaaaaaa-1111"}]}`),
	})
	assertWorkloadDestination(t, w, "aaaaaaaa")
	for i, update := range []struct{ nodes, want string }{
		{`[{"hostUuid":"aaaaaaaa-1111","name":"GPU-box"}]`, "GPU-box"},
		{`[{"hostUuid":"aaaaaaaa-1111","name":"Renamed"}]`, "Renamed"},
		{`[]`, "aaaaaaaa"},
	} {
		b.push(t, "A", i+2, "discovery:nodes-changed", json.RawMessage(update.nodes))
		l.settle(t)
		assertWorkloadDestination(t, w, update.want)
	}
}

// Malformed name updates must not replace or partially mutate a valid cache.
func TestWorkloadDestinationPreservesNamesOnMalformedNotification(t *testing.T) {
	for _, tc := range []struct {
		name, method, snapshot string
		baseline, malformed    json.RawMessage
	}{
		{
			name: "discovery wrong shape", method: "discovery:nodes-changed", snapshot: "discovery:get-nodes",
			baseline:  json.RawMessage(`{"nodes":[{"hostUuid":"aaaaaaaa-1111","name":"GPU-box"}]}`),
			malformed: json.RawMessage(`{"nodes":"invalid"}`),
		},
		{
			name: "discovery partial decode", method: "discovery:nodes-changed", snapshot: "discovery:get-nodes",
			baseline:  json.RawMessage(`{"nodes":[{"hostUuid":"aaaaaaaa-1111","name":"GPU-box"}]}`),
			malformed: json.RawMessage(`[{"hostUuid":"aaaaaaaa-1111","name":"Corrupt","port":"invalid"}]`),
		},
		{
			name: "membership wrong shape", method: "nodes:changed", snapshot: "nodes:get-initial",
			baseline:  json.RawMessage(`{"nodes":[{"nodeUuid":"aaaaaaaa-1111","name":"GPU-box"}]}`),
			malformed: json.RawMessage(`{"nodes":"invalid"}`),
		},
		{
			name: "membership partial decode", method: "nodes:changed", snapshot: "nodes:get-initial",
			baseline:  json.RawMessage(`{"nodes":[{"nodeUuid":"aaaaaaaa-1111","name":"GPU-box"}]}`),
			malformed: json.RawMessage(`{"nodes":[{"nodeUuid":"aaaaaaaa-1111","name":"Corrupt","port":"invalid"}]}`),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, l, w := workloadSession(t, map[string]json.RawMessage{
				"workloads:get-initial": json.RawMessage(`{"workloads":[{"id":"request","originatedFrom":"origin-uuid","scheduledOn":"aaaaaaaa-1111"}]}`),
				tc.snapshot:             tc.baseline,
			})
			assertWorkloadDestination(t, w, "GPU-box")
			b.push(t, "A", 2, tc.method, tc.malformed)
			l.settle(t)
			assertWorkloadDestination(t, w, "GPU-box")
			// A row rebuild must still resolve the previous name, not a cache
			// partially modified by decoding before the error was returned.
			b.push(t, "A", 3, "workloads:upsert", json.RawMessage(`{"workloadInfo":{"id":"request","originatedFrom":"origin-uuid","scheduledOn":"aaaaaaaa-1111","state":"running"}}`))
			l.settle(t)
			assertWorkloadDestination(t, w, "GPU-box")
		})
	}
}

func TestWorkloadDestinationTracksClusterMembership(t *testing.T) {
	b, l, w := workloadSession(t, map[string]json.RawMessage{
		"workloads:get-initial": json.RawMessage(`{"workloads":[{"id":"request","originatedFrom":"origin-uuid","scheduledOn":"bbbbbbbb-1111"}]}`),
	})
	b.push(t, "A", 2, "nodes:changed", json.RawMessage(`{"nodes":[{"nodeUuid":"bbbbbbbb-1111","name":"Member"}]}`))
	l.settle(t)
	assertWorkloadDestination(t, w, "Member")
	b.push(t, "A", 3, "nodes:changed", json.RawMessage(`{"nodes":[]}`))
	l.settle(t)
	assertWorkloadDestination(t, w, "bbbbbbbb")
}
