// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package routingmetrics owns the prefix-aware routing counters shared by every
// PAIR inference proxy (ollama-proxy, lmstudio-proxy, omlx-proxy).
//
// These counters used to be accumulated by each *consumer* of proxy/request: the
// desktop bridge (desktop/src/electron/service-bridge/modular-state.ts) and
// nvpair-tui (nvpair-tui/ui/proxies.go) each re-implemented the same path
// filter and the same two increments. That put routing logic in the desktop tree,
// which it must not own, guaranteed the copies would drift, and meant every new
// client had to reproduce the filter byte-for-byte to render the same numbers.
//
// A proxy has already made the routing decision by the time it emits
// proxy/request, so the filter and the totals live here — one implementation,
// reported on proxy/request and proxy:get-status. Consumers replace their
// displayed totals; they never count.
package routingmetrics

import "sync/atomic"

// inferencePaths are the endpoints whose requests feed the routing dashboard.
// Anything else — /api/tags, /api/show, /api/version, model and version probes —
// never reached a routing decision and must not appear as routed traffic.
var inferencePaths = map[string]bool{
	"/v1/chat/completions": true,
	"/v1/completions":      true,
	"/api/chat":            true,
	"/api/generate":        true,
}

// Counts reports whether a request is routable traffic. Only POST reaches a
// routing decision, so every other method is excluded regardless of path.
func Counts(method, path string) bool {
	return method == "POST" && inferencePaths[path]
}

// Counters accumulates routable requests for one proxy process. The zero value
// is ready for use, so a proxy can hold it as a plain struct field.
type Counters struct {
	requests atomic.Uint64
	hits     atomic.Uint64
}

// Routed folds one already-routed request into the totals. Callers invoke it
// only where the proxy completed a routing decision — never on a rejection path,
// which is why unrouted 502s cannot inflate the dashboard.
func (c *Counters) Routed(cacheAffinity bool) {
	c.requests.Add(1)
	if cacheAffinity {
		c.hits.Add(1)
	}
}

// Snapshot returns the cumulative (requests, cacheAffinityHits) since process
// start. Totals reset with the process, matching the lifetime of the
// per-consumer counters they replace.
func (c *Counters) Snapshot() (requests, cacheAffinityHits uint64) {
	return c.requests.Load(), c.hits.Load()
}
