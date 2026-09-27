// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package relay is the broker's discovery relay. It owns two
// halves of the star topology and is pure state + fanout logic — the broker
// binary wires it to the scanner peer (daemon) and to client connections:
//
//   - RegistrationCache: the UPWARD half. This node's advertised services,
//     registered by local workers (relayed) and by the broker's engine poller
//     (ol/lm). The broker pushes the cached set down to the daemon and replays
//     it whenever the daemon reports a new epoch (restart), so no worker needs
//     reconnect logic.
//   - Directory: the DOWNWARD half. Every LAN node, fed by the daemon's
//     discovery:node-* events, with per-subscriber filtered fanout and a
//     queryable snapshot for discovery:get-nodes.
package relay

import (
	"slices"
	"sort"
	"sync"

	"nvpair-shared/noderec"
)

// RegistrationCache holds this node's service registrations, keyed by service.
type RegistrationCache struct {
	mu   sync.Mutex
	regs map[noderec.ServiceKey]noderec.RegisterParams
}

func NewRegistrationCache() *RegistrationCache {
	return &RegistrationCache{regs: make(map[noderec.ServiceKey]noderec.RegisterParams)}
}

// Register adds or updates a service registration, reporting whether the cached
// set changed (so the broker only re-pushes to the daemon on a real change).
// update-txt is the same call with a new TXT.
func (c *RegistrationCache) Register(p noderec.RegisterParams) bool {
	if p.Service == "" || p.Port == 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if prev, ok := c.regs[p.Service]; ok && registerEqual(prev, p) {
		return false
	}
	c.regs[p.Service] = p
	return true
}

// Unregister removes a service, reporting whether it existed.
func (c *RegistrationCache) Unregister(s noderec.ServiceKey) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.regs[s]; !ok {
		return false
	}
	delete(c.regs, s)
	return true
}

// Snapshot returns the current registrations, sorted by service for a
// deterministic replay order.
func (c *RegistrationCache) Snapshot() []noderec.RegisterParams {
	c.mu.Lock()
	out := make([]noderec.RegisterParams, 0, len(c.regs))
	for _, p := range c.regs {
		out = append(out, p)
	}
	c.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

func registerEqual(a, b noderec.RegisterParams) bool {
	if a.Service != b.Service || a.Port != b.Port || len(a.TXT) != len(b.TXT) {
		return false
	}
	for i := range a.TXT {
		if a.TXT[i] != b.TXT[i] {
			return false
		}
	}
	return true
}

// Subscriber is a client interested in directory changes: a service filter and a
// callback invoked (on the caller's goroutine, under no relay lock) with the
// subscriber's full filtered node set on every change. Consumers replace their
// set from it rather than applying deltas, so a dropped or reordered push can't
// leave them drifted — every push is the authoritative current list.
type Subscriber struct {
	Filter noderec.SubscribeParams
	Send   func(nodes []noderec.DirectoryNode)

	// sendMu serializes deliveries to this subscriber so two concurrent
	// deliveries — the initial post-subscribe delivery racing an Apply fan-out
	// driven by the scanner read-pump — can't reorder and leave the subscriber
	// holding an older set than a newer one. Combined with capturing the snapshot
	// inside Deliver (at send time, not subscribe time), the last delivery to
	// acquire it always carries the latest directory state.
	sendMu sync.Mutex
}

// Source identifies whether an update comes from the network scanner or manual nodes.
type Source int

const (
	SourceScanner Source = iota
	SourceManual
)

type directoryEntry struct {
	scanner *noderec.DirectoryNode
	manual  *noderec.DirectoryNode
}

func (e directoryEntry) projected() (noderec.DirectoryNode, bool) {
	if e.scanner != nil {
		res := *e.scanner
		if e.manual != nil && len(e.manual.IPs) > 0 {
			seen := make(map[string]bool, len(res.IPs)+len(e.manual.IPs))
			for _, ip := range res.IPs {
				seen[ip] = true
			}
			merged := append([]string(nil), res.IPs...)
			for _, ip := range e.manual.IPs {
				if !seen[ip] && ip != "" {
					seen[ip] = true
					merged = append(merged, ip)
				}
			}
			res.IPs = merged
		}
		return res, true
	}
	if e.manual != nil {
		return *e.manual, true
	}
	return noderec.DirectoryNode{}, false
}

func directoryNodeEqual(a, b noderec.DirectoryNode) bool {
	if a.HostUUID != b.HostUUID || a.Name != b.Name || a.IP != b.IP || a.ClusterUUID != b.ClusterUUID || a.Trusted != b.Trusted {
		return false
	}
	if !slices.Equal(a.IPs, b.IPs) || !slices.Equal(a.Models, b.Models) {
		return false
	}
	if len(a.Services) != len(b.Services) {
		return false
	}
	for k, va := range a.Services {
		vb, ok := b.Services[k]
		if !ok || va != vb {
			return false
		}
	}
	if len(a.ModelsByEngine) != len(b.ModelsByEngine) {
		return false
	}
	for k, va := range a.ModelsByEngine {
		vb, ok := b.ModelsByEngine[k]
		if !ok || !slices.Equal(va, vb) {
			return false
		}
	}
	if len(a.LoadedByEngine) != len(b.LoadedByEngine) {
		return false
	}
	for k, va := range a.LoadedByEngine {
		vb, ok := b.LoadedByEngine[k]
		if !ok || !slices.Equal(va, vb) {
			return false
		}
	}
	if !slices.Equal(a.GPUs, b.GPUs) {
		return false
	}
	if (a.CPU == nil) != (b.CPU == nil) || (a.CPU != nil && *a.CPU != *b.CPU) {
		return false
	}
	if (a.Memory == nil) != (b.Memory == nil) || (a.Memory != nil && *a.Memory != *b.Memory) {
		return false
	}
	return true
}

// Directory is the broker's view of all LAN nodes (keyed by hostUuid) plus its
// subscriber set. It's fed by the daemon's node-* events via Apply and queried
// by discovery:get-nodes via Snapshot.
type Directory struct {
	mu      sync.Mutex
	entries map[string]directoryEntry
	nodes   map[string]noderec.DirectoryNode
	subs    map[int]*Subscriber
	nextID  int
}

func NewDirectory() *Directory {
	return &Directory{
		entries: make(map[string]directoryEntry),
		nodes:   make(map[string]noderec.DirectoryNode),
		subs:    make(map[int]*Subscriber),
	}
}

// Subscribe registers a subscriber and returns its id. The caller sends the
// initial snapshot via Deliver after releasing its own lock — Deliver captures
// the snapshot at send time, so a concurrent Apply can't sneak a newer snapshot
// in and have this initial delivery overwrite it with an older one.
func (d *Directory) Subscribe(sub *Subscriber) (id int) {
	d.mu.Lock()
	d.nextID++
	id = d.nextID
	d.subs[id] = sub
	d.mu.Unlock()
	return id
}

// Deliver pushes the subscriber its current filtered snapshot, serialized
// per-subscriber. Capturing the snapshot here (at delivery time) rather than
// handing Send a pre-captured slice means a delivery can never carry a set older
// than the directory's state when it actually runs; the per-subscriber lock then
// guarantees the initial post-subscribe delivery and a concurrent Apply fan-out
// settle on the latest set regardless of which runs last.
func (d *Directory) Deliver(sub *Subscriber) {
	sub.sendMu.Lock()
	defer sub.sendMu.Unlock()
	d.mu.Lock()
	nodes := d.filteredLocked(sub.Filter)
	d.mu.Unlock()
	sub.Send(nodes)
}

// filteredLocked returns the nodes matching a subscriber's filter, sorted by
// hostUuid for a deterministic set. Caller must hold d.mu.
func (d *Directory) filteredLocked(f noderec.SubscribeParams) []noderec.DirectoryNode {
	out := make([]noderec.DirectoryNode, 0, len(d.nodes))
	for _, n := range d.nodes {
		if f.Matches(n) {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].HostUUID < out[j].HostUUID })
	return out
}

// Unsubscribe removes a subscriber.
func (d *Directory) Unsubscribe(id int) {
	d.mu.Lock()
	delete(d.subs, id)
	d.mu.Unlock()
}

// Apply folds a daemon node-* delta into the directory (defaulting to SourceScanner),
// then re-sends every subscriber its full filtered snapshot if the directory changed.
func (d *Directory) Apply(method string, node noderec.DirectoryNode) {
	d.ApplySource(SourceScanner, method, node)
}

// ApplySource folds a node delta from a specific source (scanner or manual) into the
// directory. When both sources report the same hostUuid, the scanner view takes
// precedence (authoritative hostname/mDNS identity) with manual addresses merged as
// fallbacks, preventing oscillation. Deliveries to subscribers are only made when
// the projected node state actually changes.
func (d *Directory) ApplySource(source Source, method string, node noderec.DirectoryNode) {
	if node.HostUUID == "" {
		return
	}
	d.mu.Lock()
	entry := d.entries[node.HostUUID]
	prevNode, hadPrev := d.nodes[node.HostUUID]

	if method == noderec.NotifyNodeRemoved {
		if source == SourceScanner {
			entry.scanner = nil
		} else {
			entry.manual = nil
		}
	} else {
		cp := node
		if source == SourceScanner {
			entry.scanner = &cp
		} else {
			entry.manual = &cp
		}
	}

	proj, exists := entry.projected()
	var changed bool
	if exists {
		d.entries[node.HostUUID] = entry
		d.nodes[node.HostUUID] = proj
		changed = !hadPrev || !directoryNodeEqual(prevNode, proj)
	} else {
		delete(d.entries, node.HostUUID)
		delete(d.nodes, node.HostUUID)
		changed = hadPrev
	}

	if !changed {
		d.mu.Unlock()
		return
	}

	subs := make([]*Subscriber, 0, len(d.subs))
	for _, s := range d.subs {
		subs = append(subs, s)
	}
	d.mu.Unlock()

	// Deliver captures each subscriber's snapshot at send time (under its
	// per-subscriber lock), so this fan-out and a concurrent initial delivery
	// can't reorder into a stale set.
	for _, s := range subs {
		d.Deliver(s)
	}
}

// Snapshot returns the directory optionally filtered to one service, sorted by
// hostUuid.
func (d *Directory) Snapshot(filter noderec.ServiceKey) []noderec.DirectoryNode {
	d.mu.Lock()
	out := make([]noderec.DirectoryNode, 0, len(d.nodes))
	for _, n := range d.nodes {
		if filter != "" && !n.HasService(filter) {
			continue
		}
		out = append(out, n)
	}
	d.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].HostUUID < out[j].HostUUID })
	return out
}
