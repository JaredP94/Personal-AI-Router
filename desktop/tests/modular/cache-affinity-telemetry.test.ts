// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import type { JsonValue } from '@/shared/types/json'
import { subscribePush } from '@/electron/service-bridge/push-bus'
import { getModularBridgeState } from '@/electron/service-bridge/modular-state'

describe('cache affinity diagnostics', () => {
    it('counts only completed inference notifications carrying affinity telemetry, per engine', () => {
        const state = getModularBridgeState()
        const before = state.getRoutingMetrics().find(row => row.engineType === 'omlx')
        let updates = 0
        const unsubscribe = subscribePush(event => {
            if (event.channel === 'metrics:routing-update') updates += 1
        })
        const events: JsonValue[] = [
            { method: 'POST', path: '/v1/chat/completions', cache_affinity: true },
            { method: 'POST', path: '/v1/completions', cache_affinity: false },
            { method: 'GET', path: '/v1/models', cache_affinity: true },
            { method: 'POST', path: '/v1/chat/completions' },
            { method: 'POST', path: '/v1/embeddings', cache_affinity: true }
        ]
        for (const params of events) {
            state.handleNotification({ source: 'omlx-proxy', method: 'proxy/request', params })
        }
        state.handleNotification({
            source: 'omlx-proxy',
            method: 'proxy/request-started',
            params: { method: 'POST', path: '/v1/chat/completions', cache_affinity: true }
        })
        unsubscribe()
        expect(updates).toBe(2)
        expect(state.getRoutingMetrics().find(row => row.engineType === 'omlx')).toEqual({
            engineType: 'omlx',
            requests: (before?.requests ?? 0) + 2,
            cacheAffinityHits: (before?.cacheAffinityHits ?? 0) + 1
        })
        expect(state.getRoutingMetrics().find(row => row.engineType === 'ollama')).toMatchObject({
            requests: 0,
            cacheAffinityHits: 0
        })
    })
})
