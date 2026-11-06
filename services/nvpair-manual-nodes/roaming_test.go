// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClusterSeedFollowsRoamingAddressWithoutOverwritingSavedEntry(t *testing.T) {
	for _, saved := range []bool{false, true} {
		name := "cluster seed"
		if saved {
			name = "saved alias"
		}
		t.Run(name, func(t *testing.T) {
			m, _, _ := newTestManager(t)
			m.clusterDir = t.TempDir()
			const uuid = "22222222-2222-2222-2222-222222222222"
			write := func(addr string, tick int) {
				t.Helper()
				data, err := json.Marshal([]clusterMemberEntry{{ID: "peer", Name: "peer", NodeUUID: uuid, IPAddress: addr, State: "member"}})
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(m.clusterDir, "members.json")
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				at := time.Now().Add(time.Duration(tick) * time.Second)
				if err := os.Chtimes(path, at, at); err != nil {
					t.Fatal(err)
				}
				m.syncClusterMembers()
			}
			// Start with a previously proven tracked peer, without a background
			// probe racing the address-change interleaving being tested.
			entry := ManualEntry{Kind: "pair", Address: "192.0.2.20", Name: "peer"}
			if saved {
				entry.Name = "my-saved-peer"
			}
			m.mu.Lock()
			m.nodes[nodeID(entry)] = &trackedNode{entry: entry, fromCluster: !saved, status: ManualNodeStatus{ID: nodeID(entry), Kind: "pair", Address: entry.Address, HostUUID: uuid, ClusterUUID: uuid, FromCluster: !saved}}
			m.mu.Unlock()
			write("100.64.0.8", 2)
			nodes := m.listNodes()
			if len(nodes) != 1 {
				t.Fatalf("nodes=%+v", nodes)
			}
			want := "100.64.0.8"
			if saved {
				want = "192.0.2.20"
			}
			if nodes[0].Address != want {
				t.Fatalf("reached address=%q want %q", nodes[0].Address, want)
			}
		})
	}
}
