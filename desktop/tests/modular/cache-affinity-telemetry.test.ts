// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import type { JsonValue } from '@/shared/types/json'
import { subscribePush } from '@/electron/service-bridge/push-bus'
import { getModularBridgeState } from '@/electron/service-bridge/modular-state'

// The routing filter and its totals live in the proxies (nvpair-shared/routingmetrics).
// The bridge caches what a proxy reports and replaces it wholesale — it does not
// count. The previous version of this test fed five events with no totals and
// asserted the bridge had accumulated 2 requests / 1 hit, which proved the bridge
// re-implemented the routing filter here (forbidden by AGENTS.md) and counted
// unrouted traffic: a GET model list and a cache_affinity-less POST — the shape
// of a 502 rejection — were both counted as routed requests.
describe('cache affinity diagnostics', () => {
    it('caches the authoritative totals a proxy reports, per engine', () => {
        const state = getModularBridgeState()
        let updates = 0
        const unsubscribe = subscribePush(event => {
            if (event.channel === 'metrics:routing-update') updates += 1
        })

        const routed: JsonValue = {
            method: 'POST',
            path: '/v1/chat/completions',
            cache_affinity: true,
            requests: 4,
            cache_affinity_hits: 3
        }
        state.handleNotification({
            source: 'omlx-proxy',
            method: 'proxy/request',
            params: routed
        })

        expect(state.getRoutingMetrics().find(row => row.engineType === 'omlx')).toEqual({
            engineType: 'omlx',
            requests: 4,
            cacheAffinityHits: 3
        })
        // Totals replace, so a lower reported total is adopted rather than
        // max()-ed — this is what a proxy restart looks like to the bridge.
        state.handleNotification({
            source: 'omlx-proxy',
            method: 'proxy/request',
            params: {
                method: 'POST',
                path: '/api/generate',
                cache_affinity: false,
                requests: 1,
                cache_affinity_hits: 0
            } satisfies JsonValue
        })
        expect(state.getRoutingMetrics().find(row => row.engineType === 'omlx')).toEqual({
            engineType: 'omlx',
            requests: 1,
            cacheAffinityHits: 0
        })
        expect(updates).toBe(2)

        // An unrouted event (502 shape: no totals) neither updates nor pushes.
        state.handleNotification({
            source: 'omlx-proxy',
            method: 'proxy/request',
            params: {
                method: 'POST',
                path: '/v1/chat/completions',
                status: 502,
                error: 'no available node'
            } satisfies JsonValue
        })
        expect(state.getRoutingMetrics().find(row => row.engineType === 'omlx')).toEqual({
            engineType: 'omlx',
            requests: 1,
            cacheAffinityHits: 0
        })
        expect(updates).toBe(2)

        // Totals must not leak across engines.
        expect(state.getRoutingMetrics().find(row => row.engineType === 'ollama')).toMatchObject({
            requests: 0,
            cacheAffinityHits: 0
        })
        expect(state.getRoutingMetrics().find(row => row.engineType === 'lm-studio')).toMatchObject(
            {
                requests: 0,
                cacheAffinityHits: 0
            }
        )

        // Only proxy/request drives the channel.
        state.handleNotification({
            source: 'omlx-proxy',
            method: 'proxy/request-started',
            params: { method: 'POST', path: '/v1/chat/completions', cache_affinity: true }
        })
        expect(updates).toBe(2)

        unsubscribe()
    })
})
