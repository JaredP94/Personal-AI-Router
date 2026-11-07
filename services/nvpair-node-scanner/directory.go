// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"reflect"
	"sort"
	"sync"
	"time"

	"nvpair-shared/enginenative"
	"nvpair-shared/netpick"
	"nvpair-shared/noderec"
)

// toDirectoryNode converts a browsed _nvpair-node instance into a directory entry
// — identity + advertised services — without enrichment or liveness (those
// decorate it later in the daemon). Proven records use authenticated hostUuid;
// previews use the advertisement endpoint identity. Name is the mDNS instance
// (hostname). Every browse record starts as an independently keyed unverified
// preview; only a completed authenticated inventory read can promote a peer.
//
// Addresses are carried through whole rather than reduced to one. The node ranked
// its own addresses from evidence no observer has — which of its interfaces the
// kernel routes off-link, which carry live peers, which ones peers have connected
// to — so IPs keeps that order and IP is simply its head. Collapsing here is what
// left every consumer with a single address and no recourse when it turned out to
// be a link only one other machine could reach.
//
// It returns ok=false for a record without a uuid= TXT: a conformant
// _nvpair-node record always carries one (the daemon stamps it from
// nodeid.Resolve), so a record without it can't be keyed by a stable identity.
// We skip it (and warn) rather than key it by the mutable, non-unique hostname —
// node identity is the UUID everywhere, with no hostname fallback.
func toDirectoryNode(raw RawNode) (noderec.DirectoryNode, bool) {
	rec := noderec.ParseTXT(raw.TXT)
	if rec.HostUUID == "" {
		slog.Warn("skipping _nvpair-node record without uuid=; cannot key it by a stable identity",
			"instance", raw.ID, "addrs", raw.Addresses)
		return noderec.DirectoryNode{}, false
	}
	// netpick.Candidates unions the record's own ip= / ips= with whatever the
	// browse resolved, preserving the node's order and appending the rest.
	ips := netpick.Candidates(raw.TXT, raw.Addresses)
	var ip string
	if len(ips) > 0 {
		ip = ips[0]
	}
	svcs := make(map[noderec.ServiceKey]noderec.ServiceStatus, len(rec.Services))
	for k, port := range rec.Services {
		svcs[k] = noderec.ServiceStatus{Port: port}
	}
	return noderec.DirectoryNode{
		HostUUID:             rec.HostUUID,
		Name:                 raw.ID,
		IP:                   ip,
		IPs:                  ips,
		ClusterUUID:          rec.ClusterUUID,
		Trusted:              false,
		IdentityVerification: "unverified",
		PeerRecordID:         candidateRecordKey(raw),
		Services:             svcs,
		// Models is enriched separately from engine-manager's em /v1/models
		// (daemon.enrichModelsAt), not carried in the record's TXT.
		LastSeen: time.Now().Unix(),
	}, true
}

// directory is the daemon's view of all LAN nodes (this node included), keyed by
// authenticated hostUuid or independent preview key. Mutations come from the
// browse-event goroutine; reads come from
// discovery:get-nodes handlers, so it's an RWMutex map.
type directory struct {
	mu    sync.RWMutex
	nodes map[string]noderec.DirectoryNode
}

func newDirectory() *directory {
	return &directory{nodes: make(map[string]noderec.DirectoryNode)}
}

// upsert stores a node and reports whether it was new (vs. an update).
//
// The stored engine-native view is carried across the replacement: a record
// built from the browse path carries no view (the mDNS TXT has none), so a
// plain replacement would delete the view the sweep had stored, and the client
// would watch the field disappear and reappear on every advertisement. The
// models field survives the same replacement through the enrichment cache;
// this is the same rule for the engine's own view.
func (d *directory) upsert(n noderec.DirectoryNode) (isNew bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := n.RecordKey()
	if key == "" {
		return false
	}
	_, existed := d.nodes[key]
	d.nodes[key] = carryEngineNative(d.nodes[key], n)
	return !existed
}

// carryEngineNative keeps the engine-native view a stored record already held
// when the record arriving in its place carries none.
//
// A node that has never produced a view has none, and a node whose view is
// deliberately replaced by a record that carries one takes the new one: only
// the absent case is carried forward.
func carryEngineNative(stored, incoming noderec.DirectoryNode) noderec.DirectoryNode {
	if incoming.ServicePeer() && incoming.PeerRecordID == stored.PeerRecordID && incoming.EngineNative == nil && stored.EngineNative != nil {
		incoming.EngineNative = stored.EngineNative
	}
	return incoming
}

// replaceIfCurrent prevents a completed probe from overwriting a newer record.
func (d *directory) replaceIfCurrent(old, replacement noderec.DirectoryNode) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	current, ok := d.nodes[old.RecordKey()]
	if !ok || !reflect.DeepEqual(current, old) {
		return false
	}
	delete(d.nodes, old.RecordKey())
	d.nodes[replacement.RecordKey()] = replacement
	return true
}

// removeIfCurrent atomically removes only the exact snapshot that expired.
// A refresh within the same LastSeen second is still a different record.
func (d *directory) removeIfCurrent(old noderec.DirectoryNode) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	current, ok := d.nodes[old.RecordKey()]
	if !ok || !reflect.DeepEqual(current, old) {
		return false
	}
	delete(d.nodes, old.RecordKey())
	return true
}

// remove drops a node by hostUuid and reports whether it existed.
func (d *directory) remove(hostUUID string) (existed bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, existed = d.nodes[hostUUID]
	delete(d.nodes, hostUUID)
	return existed
}

// get returns a node by hostUuid.
func (d *directory) get(hostUUID string) (noderec.DirectoryNode, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	n, ok := d.nodes[hostUUID]
	return n, ok
}

// snapshot returns the directory, optionally filtered to nodes advertising a
// given service, sorted by hostUuid for stable rendering. An empty filter
// returns every node.
func (d *directory) snapshot(filter noderec.ServiceKey) []noderec.DirectoryNode {
	d.mu.RLock()
	out := make([]noderec.DirectoryNode, 0, len(d.nodes))
	for _, n := range d.nodes {
		if filter != "" && !n.HasService(filter) {
			continue
		}
		out = append(out, n)
	}
	d.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].HostUUID < out[j].HostUUID })
	return out
}

// applyModels atomically reconciles one node's model inventory from a completed
// /v1/models fetch, guarded against a node that was removed or re-addressed
// while the fetch was in flight. It is the periodic refresh loop's write path
// (daemon.refreshNodeModels); the event-driven enrichment writes the whole node
// via upsert instead.
//
// It confirms the node still exists AND still advertises the same em endpoint
// (ip + port) the fetch targeted, so an in-flight result can never resurrect a
// removed node or overwrite one that has since been re-addressed. It then
// compares the fetched flat, per-engine, and loaded inventories
// order-insensitively with the stored entry and, only when any changed, updates
// just the model fields in place — preserving every other field (identity,
// services, node-info, trust) the browse path owns.
//
// Returns the (possibly updated) node, whether the inventory changed, and
// whether the guarded apply was valid. ok == false means the result is stale
// (node gone or re-addressed) and the caller must not cache or emit it.
func (d *directory) applyModels(hostUUID, ip string, emPort int, models []string, byEngine, loadedByEngine map[string][]string, downloadedByEngine map[string][]noderec.DownloadedModel) (node noderec.DirectoryNode, changed, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n, present := d.nodes[hostUUID]
	if !present {
		return noderec.DirectoryNode{}, false, false
	}
	em, has := n.Services[noderec.ServiceEngineManager]
	if !has || n.IP != ip || em.Port != emPort {
		return n, false, false
	}
	if sameStringSet(n.Models, models) && sameByEngine(n.ModelsByEngine, byEngine) && sameByEngine(n.LoadedByEngine, loadedByEngine) && reflect.DeepEqual(n.DownloadedByEngine, downloadedByEngine) {
		return n, false, true
	}
	n.Models = models
	n.ModelsByEngine = byEngine
	n.LoadedByEngine = loadedByEngine
	n.DownloadedByEngine = downloadedByEngine
	d.nodes[hostUUID] = n
	return n, true, true
}

// applyEngineNative atomically reconciles one node's engine-native view from a
// completed /v1/engine-stats fetch, guarded against a node that was removed or
// re-addressed while the fetch was in flight. It is the engine-native sweep's
// write path (daemon.refreshNodeEngineNative).
//
// It confirms the node still exists AND still advertises the same em endpoint
// (ip + port) the fetch targeted, so an in-flight result can never resurrect a
// removed node or overwrite one that has since been re-addressed. It then
// compares the fetched view with the stored one and updates the field in
// place — preserving every other field (identity, services, node-info, trust)
// the browse path owns.
//
// The change is decided by two questions, kept apart: whether the engine's
// numbers moved, and whether the observation is newer. A newer observation is
// a change worth reporting, because the stamp is what tells a reader how old
// the reading is; the fresh view is stored either way, so the scanner's own
// record carries the latest reading.
//
// A nil view is a fetch that failed: the node keeps the view it had (the
// last-good rule the model list follows too), and no change is reported.
//
// Returns the (possibly updated) node, whether the view changed, and whether the
// guarded apply was valid. ok == false means the result is stale (node gone or
// re-addressed) and the caller must not emit it.
func (d *directory) applyEngineNative(hostUUID, ip string, emPort int, stats *enginenative.Stats) (node noderec.DirectoryNode, changed, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n, present := d.nodes[hostUUID]
	if !present {
		return noderec.DirectoryNode{}, false, false
	}
	em, has := n.Services[noderec.ServiceEngineManager]
	if !has || n.IP != ip || em.Port != emPort {
		return n, false, false
	}
	if stats == nil {
		// A failed fetch leaves the node with the view it had.
		return n, false, true
	}
	moved := enginenative.Changed(n.EngineNative, stats)
	n.EngineNative = stats
	d.nodes[hostUUID] = n
	return n, moved, true
}

// sameByEngine reports whether two per-engine model maps are semantically equal:
// the same set of engine keys, each mapping to the same set of model names
// (order-insensitive). A nil and an empty map compare equal (both mean "no
// per-engine attribution").
func sameByEngine(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || !sameStringSet(av, bv) {
			return false
		}
	}
	return true
}
