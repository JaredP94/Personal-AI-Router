// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"nvpair-shared/jsonrpc"
	"nvpair-shared/noderec"
)

type burstNotification struct {
	method string
	params json.RawMessage
}

// Synthetic startup fixture, not a claim about the largest possible cluster:
// two lifecycle transitions per worker, 128 node deltas and a full directory.
func notificationBurst() []burstNotification {
	b := &Broker{}
	b.initWorkerRegistry()
	workers := b.workerSnapshot()
	for i := range workers.Workers {
		workers.Workers[i].Since = time.Unix(0, 0).UTC()
		workers.Workers[i].LastError = ""
	}
	var burst []burstNotification
	for range 24 {
		raw, _ := json.Marshal(workers)
		burst = append(burst, burstNotification{"workers:update", raw})
	}
	nodes := make([]noderec.DirectoryNode, 128)
	for i := range nodes {
		models := make([]string, 16)
		for j := range models {
			models[j] = fmt.Sprintf("fixture-model-%02d", j)
		}
		nodes[i] = noderec.DirectoryNode{
			HostUUID: fmt.Sprintf("00000000-0000-0000-0000-%012d", i), Name: "fixture-node", IP: "192.0.2.1",
			ClusterUUID: "fixture-cluster", PeerRecordID: "fixture-proof", IdentityVerification: "verified", Trusted: true,
			Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceCluster: {Port: 14321}, noderec.ServiceEngineManager: {Port: 14322}},
			Models:   models,
		}
		raw, _ := json.Marshal(nodes[i])
		burst = append(burst, burstNotification{noderec.NotifyNodeDiscovered, raw})
	}
	raw, _ := json.Marshal(noderec.GetNodesResult{Nodes: nodes})
	return append(burst, burstNotification{noderec.NotifyNodes, raw})
}

func measureNotificationBurst(tb testing.TB, burst []burstNotification) (int, int, time.Duration) {
	tb.Helper()
	local, remote := net.Pipe()
	c := NewCodec(local)
	defer c.Close()
	defer remote.Close()
	received := make(chan []byte, 1)
	go func() { var buf bytes.Buffer; _, _ = io.Copy(&buf, remote); received <- buf.Bytes() }()
	start := time.Now()
	for _, frame := range burst {
		if err := c.Notify(frame.method, frame.params); err != nil {
			tb.Fatal(err)
		}
	}
	if err := c.Flush(context.Background()); err != nil {
		tb.Fatal(err)
	}
	elapsed := time.Since(start)
	_ = local.Close()
	wire := <-received
	return bytes.Count(wire, []byte{'\n'}), len(wire), elapsed
}

func TestNotificationBurstBudget(t *testing.T) {
	frames, size, elapsed := measureNotificationBurst(t, notificationBurst())
	t.Logf("startup fixture: %d frames, %d bytes, drain %s", frames, size, elapsed)
	if frames != 153 || frames >= jsonrpc.WriteQueueFrames || size >= jsonrpc.WriteQueueBytes {
		t.Fatalf("fixture no longer fits admission budget: %d frames, %d bytes", frames, size)
	}
}

func BenchmarkNotificationBurst(b *testing.B) {
	burst := notificationBurst()
	b.ResetTimer()
	for range b.N {
		_, size, _ := measureNotificationBurst(b, burst)
		b.SetBytes(int64(size))
	}
}
