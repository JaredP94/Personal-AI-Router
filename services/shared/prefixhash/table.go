// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package prefixhash

import (
	"container/list"
	"sync"
	"time"
)

// RouteRecord identifies a successful destination and its last hint access.
type RouteRecord struct {
	NodeID     string
	Engine     string
	LastUsedAt time.Time
}

type routeKey struct {
	model  string
	digest Digest
}

type entry struct {
	key    routeKey
	record RouteRecord
}

// PrefixRoutingTable retains only model names, hashes, and route metadata.
// Access and replacement refresh the idle TTL and LRU position atomically.
type PrefixRoutingTable struct {
	mu       sync.Mutex
	entries  map[routeKey]*list.Element
	recent   list.List
	capacity int
	ttl      time.Duration
}

// NewPrefixRoutingTable defaults nonpositive capacity and TTL to 10,000 entries
// and five minutes, respectively.
func NewPrefixRoutingTable(capacity int, ttl time.Duration) *PrefixRoutingTable {
	if capacity <= 0 {
		capacity = 10000
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &PrefixRoutingTable{entries: make(map[routeKey]*list.Element), capacity: capacity, ttl: ttl}
}

// Lookup returns a live record and refreshes its idle lifetime.
func (t *PrefixRoutingTable) Lookup(model string, digest Digest) (RouteRecord, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	element, ok := t.entries[routeKey{model: model, digest: digest}]
	if !ok {
		return RouteRecord{}, false
	}
	value := element.Value.(*entry)
	now := time.Now()
	if now.Sub(value.record.LastUsedAt) >= t.ttl {
		t.remove(element)
		return RouteRecord{}, false
	}
	value.record.LastUsedAt = now
	t.recent.MoveToFront(element)
	return value.record, true
}

// Put records successful routing. The table owns timestamps; caller-supplied
// LastUsedAt cannot extend a hint indefinitely or expire it prematurely.
func (t *PrefixRoutingTable) Put(model string, digest Digest, record RouteRecord) {
	if model == "" || digest == (Digest{}) || record.NodeID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for oldest := t.recent.Back(); oldest != nil; oldest = t.recent.Back() {
		if now.Sub(oldest.Value.(*entry).record.LastUsedAt) < t.ttl {
			break
		}
		t.remove(oldest)
	}
	record.LastUsedAt = now
	key := routeKey{model: model, digest: digest}
	if element, ok := t.entries[key]; ok {
		element.Value.(*entry).record = record
		t.recent.MoveToFront(element)
		return
	}
	if len(t.entries) >= t.capacity {
		t.remove(t.recent.Back())
	}
	t.entries[key] = t.recent.PushFront(&entry{key: key, record: record})
}

func (t *PrefixRoutingTable) remove(element *list.Element) {
	delete(t.entries, element.Value.(*entry).key)
	t.recent.Remove(element)
}
