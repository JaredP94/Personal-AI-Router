// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "nvpair-shared/routingmetrics"

// recordRoutingMetric folds one *routed* request into this proxy's shared
// counters, and returns the cumulative totals to stamp on the event.
//
// The filter and the accumulation live in nvpair-shared/routingmetrics, not
// here: ollama-proxy, lmstudio-proxy and omlx-proxy must report identical
// numbers, and that package is where the one definition lives.
//
// Called only at the completion emit site, before the notification is written,
// so the event carries the total that includes itself. The 502 rejection path
// (proxy.go ~1088) never calls this, which is what keeps unrouted traffic out
// of the dashboard — structurally, not by inspecting a field. That site is
// precisely what the old consumer-side filter mis-counted, because a rejection
// emits on an inference path with cache_affinity omitted and was counted as a
// routed request.
func (p *Proxy) recordRoutingMetric(method, path string, cacheAffinity bool) (requests, cacheAffinityHits uint64) {
	if routingmetrics.Counts(method, path) {
		p.routing.Routed(cacheAffinity)
	}
	return p.routing.Snapshot()
}
