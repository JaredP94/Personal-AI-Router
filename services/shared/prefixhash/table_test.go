// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package prefixhash

import (
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestTableLRUAndModelIsolation(t *testing.T) {
	table := NewPrefixRoutingTable(2, time.Minute)
	table.Put("a", Digest{1}, RouteRecord{NodeID: "first", Engine: "omlx"})
	table.Put("b", Digest{1}, RouteRecord{NodeID: "second"})
	if got, ok := table.Lookup("a", Digest{1}); !ok || got.NodeID != "first" || got.Engine != "omlx" || got.LastUsedAt.IsZero() {
		t.Fatal("record metadata or model isolation lost")
	}
	table.Put("a", Digest{2}, RouteRecord{NodeID: "third"})
	if _, ok := table.Lookup("b", Digest{1}); ok {
		t.Fatal("least recently accessed record was not evicted")
	}
	table.Put("a", Digest{1}, RouteRecord{NodeID: "updated"})
	if got, ok := table.Lookup("a", Digest{1}); !ok || got.NodeID != "updated" {
		t.Fatal("existing record was not updated")
	}
	if _, ok := table.Lookup("a", Digest{2}); !ok {
		t.Fatal("updating a record consumed extra capacity")
	}
}

func TestTableTTLRefreshAndDefaults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		table := NewPrefixRoutingTable(0, 0)
		table.Put("m", Digest{1}, RouteRecord{NodeID: "node", LastUsedAt: time.Unix(1, 0)})
		time.Sleep(4 * time.Minute)
		if _, ok := table.Lookup("m", Digest{1}); !ok {
			t.Fatal("default TTL expired early")
		}
		time.Sleep(4 * time.Minute)
		if _, ok := table.Lookup("m", Digest{1}); !ok {
			t.Fatal("lookup failed to refresh idle TTL")
		}
		time.Sleep(5 * time.Minute)
		if _, ok := table.Lookup("m", Digest{1}); ok {
			t.Fatal("idle record survived default TTL")
		}
	})
}

func TestTableIgnoresEmptyKeysAndIsConcurrent(t *testing.T) {
	table := NewPrefixRoutingTable(16, time.Minute)
	table.Put("", Digest{1}, RouteRecord{NodeID: "node"})
	table.Put("m", Digest{}, RouteRecord{NodeID: "node"})
	table.Put("m", Digest{2}, RouteRecord{})
	if _, ok := table.Lookup("", Digest{1}); ok {
		t.Fatal("empty model recorded")
	}
	if _, ok := table.Lookup("m", Digest{}); ok {
		t.Fatal("empty digest recorded")
	}
	if _, ok := table.Lookup("m", Digest{2}); ok {
		t.Fatal("empty destination recorded")
	}
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 100 {
				digest := Digest{byte(i), byte(j), 1}
				table.Put("m", digest, RouteRecord{NodeID: "node"})
				table.Lookup("m", digest)
			}
		}()
	}
	wg.Wait()
}

func TestTableConfiguredExpiryPrunesBeforeCapacityEviction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		table := NewPrefixRoutingTable(2, time.Second)
		table.Put("m", Digest{1}, RouteRecord{NodeID: "expired"})
		time.Sleep(500 * time.Millisecond)
		table.Put("m", Digest{2}, RouteRecord{NodeID: "live"})
		time.Sleep(500 * time.Millisecond)
		table.Put("m", Digest{3}, RouteRecord{NodeID: "new"})
		if _, ok := table.Lookup("m", Digest{1}); ok {
			t.Fatal("expired record retained")
		}
		for _, digest := range []Digest{{2}, {3}} {
			if _, ok := table.Lookup("m", digest); !ok {
				t.Fatal("live record lost while pruning")
			}
		}
	})
}
