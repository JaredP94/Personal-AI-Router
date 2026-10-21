// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
    formatDurationMs,
    formatTokenCount,
    formatTokensPerSecond,
    hasPhaseMetrics
} from '@/ui/utils/inference-metrics'
import type { InferenceMetricsTotals, NodeInferenceMetricsSnapshot } from '@/shared/types/metrics'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import {
    parseNodeMetricsSnapshot,
    parseWorkloadsInitial
} from '@/electron/service-bridge/modular-state'
import { useMetricsStore } from '@/ui/stores/metrics.store'

/** A node whose engine reported every phase. */
const reportedTotals: InferenceMetricsTotals = {
    requests: 3,
    prefillTokens: 100,
    decodeTokens: 20,
    cachedTokens: 0,
    prefillMs: 1000,
    decodeMs: 2000,
    ttftMs: 300,
    ttftRequests: 3,
    tokenRequests: 3,
    prefillRequests: 1,
    decodeRequests: 1
}

/** A node whose engine reported nothing at all. */
const silentTotals: InferenceMetricsTotals = {
    requests: 3,
    prefillTokens: 0,
    decodeTokens: 0,
    cachedTokens: 0,
    prefillMs: 0,
    decodeMs: 0,
    ttftMs: 0,
    ttftRequests: 0,
    tokenRequests: 0,
    prefillRequests: 0,
    decodeRequests: 0
}

const snapshot: NodeInferenceMetricsSnapshot = {
    nodes: [
        {
            nodeId: 'node-a',
            totals: reportedTotals,
            rates: {
                prefillTokensPerSecond: 100,
                decodeTokensPerSecond: 10,
                meanTtftMs: 100
            },
            models: [{ model: 'llama-3.2-3b', totals: reportedTotals }]
        }
    ]
}

describe('inference metric formatting', () => {
    it('distinguishes an absent phase from a measured zero', () => {
        expect(formatDurationMs(null)).toBe('not reported')
        expect(formatDurationMs(undefined)).toBe('not reported')
        expect(formatDurationMs(0)).toBe('0 ms')
        expect(formatDurationMs(1500)).toBe('1.5 s')
    })

    it('formats token counts and rates', () => {
        expect(formatTokenCount(0)).toBe('0')
        expect(formatTokenCount(1200)).toBe('1,200')
        expect(formatTokensPerSecond(829.3)).toBe('829 tok/s')
        expect(formatTokensPerSecond(0)).toBe('0 tok/s')
    })

    it('reports a missing rate as not reported rather than as zero', () => {
        expect(formatTokensPerSecond(null)).toBe('not reported')
        expect(formatTokensPerSecond(undefined)).toBe('not reported')
        expect(formatTokenCount(null)).toBe('not reported')
    })

    it('reports whether any phase was measured', () => {
        expect(hasPhaseMetrics(silentTotals)).toBe(false)
        expect(hasPhaseMetrics({ ...silentTotals, prefillMs: 10 })).toBe(true)
    })

    it('counts a reported zero-length phase as measured', () => {
        // The engine reported a prefill phase that took no time. The presence
        // counter says so; the duration alone cannot.
        expect(hasPhaseMetrics({ ...silentTotals, prefillRequests: 1 })).toBe(true)
        expect(hasPhaseMetrics({ ...silentTotals, decodeRequests: 1 })).toBe(true)
    })
})

describe('per-node inference metrics parsing', () => {
    it('keeps the broker totals, rates and per-model rows verbatim', () => {
        const parsed = parseNodeMetricsSnapshot({
            nodes: [
                {
                    nodeId: 'node-a',
                    totals: { ...reportedTotals },
                    rates: {
                        prefillTokensPerSecond: 829.3,
                        decodeTokensPerSecond: 173.1,
                        meanTtftMs: 316.7
                    },
                    models: [{ model: 'llama-3.2-3b', totals: { ...reportedTotals } }]
                }
            ]
        })
        expect(parsed.nodes).toHaveLength(1)
        expect(parsed.nodes[0].nodeId).toBe('node-a')
        expect(parsed.nodes[0].totals).toEqual(reportedTotals)
        expect(parsed.nodes[0].rates.prefillTokensPerSecond).toBe(829.3)
        expect(parsed.nodes[0].models[0].model).toBe('llama-3.2-3b')
        expect(parsed.nodes[0].models[0].totals).toEqual(reportedTotals)
    })

    it('keeps a zero presence counter at zero so the UI can read it as unreported', () => {
        const parsed = parseNodeMetricsSnapshot({
            nodes: [
                {
                    nodeId: 'node-a',
                    totals: { ...silentTotals },
                    rates: {
                        prefillTokensPerSecond: 0,
                        decodeTokensPerSecond: 0,
                        meanTtftMs: 0
                    },
                    models: []
                }
            ]
        })
        expect(parsed.nodes[0].totals.prefillRequests).toBe(0)
        expect(parsed.nodes[0].totals.tokenRequests).toBe(0)
        expect(parsed.nodes[0].rates.prefillTokensPerSecond).toBe(0)
    })

    it('drops an entry with no node id and never invents a node', () => {
        const parsed = parseNodeMetricsSnapshot({
            nodes: [{ totals: { ...reportedTotals }, rates: {}, models: [] }]
        })
        expect(parsed.nodes).toEqual([])
    })

    it('answers an empty snapshot for a malformed payload', () => {
        expect(parseNodeMetricsSnapshot(undefined)).toEqual({ nodes: [] })
        expect(parseNodeMetricsSnapshot({ nodes: 'nope' })).toEqual({ nodes: [] })
    })
})

describe('the per-job inference measurement', () => {
    it('carries the reported values and leaves an unreported value null', () => {
        const workloads = parseWorkloadsInitial({
            workloads: [
                {
                    id: 'w1',
                    model: 'llama-3.2-3b',
                    engine: 'ollama',
                    state: 'completed',
                    originatedFrom: 'node-a',
                    createdAt: 1,
                    prefillTokens: 10,
                    decodeTokens: 0,
                    cachedTokens: 0,
                    prefillMs: 120,
                    decodeMs: 900,
                    ttftMs: 80,
                    totalMs: 1020
                },
                {
                    id: 'w2',
                    model: 'llama-3.2-3b',
                    engine: 'ollama',
                    state: 'completed',
                    originatedFrom: 'node-a',
                    createdAt: 2
                }
            ]
        })
        expect(workloads).toHaveLength(2)
        expect(workloads[0].prefillTokens).toBe(10)
        // A measured zero stays a zero: the engine reported no output tokens.
        expect(workloads[0].decodeTokens).toBe(0)
        expect(workloads[0].ttftMs).toBe(80)
        expect(workloads[0].totalMs).toBe(1020)
        // An engine that reported nothing leaves the value null — "not
        // reported by this engine", which is not a measured zero.
        expect(workloads[1].prefillTokens).toBeNull()
        expect(workloads[1].ttftMs).toBeNull()
        expect(workloads[1].totalMs).toBeNull()
    })
})

describe('the metrics store holds the per-node inference metrics', () => {
    let push: (snapshot: NodeInferenceMetricsSnapshot) => void = () => {}
    let getNodeMetrics: ReturnType<typeof vi.fn>

    beforeEach(() => {
        vi.useFakeTimers()
        // Reset before the window exists: the reset itself does not read.
        useMetricsStore.getState().clearAll()
        getNodeMetrics = vi.fn(async () => snapshot)
        vi.stubGlobal('window', {
            pairApi: {
                metrics: {
                    onRoutingUpdate: () => () => {},
                    getRouting: async () => [],
                    onUpdate: () => () => {},
                    getNodeMetrics,
                    onNodeMetrics: (callback: (next: NodeInferenceMetricsSnapshot) => void) => {
                        push = callback
                        return () => {
                            push = () => {}
                        }
                    }
                },
                nodes: { onRemove: () => () => {} }
            }
        })
        useMetricsStore.getState().initialize()
    })

    afterEach(() => {
        useMetricsStore.getState().cleanup()
        vi.unstubAllGlobals()
        vi.useRealTimers()
    })

    it('keys the snapshot by node id', async () => {
        await vi.advanceTimersByTimeAsync(0)
        expect(getNodeMetrics).toHaveBeenCalled()
        const stored = useMetricsStore.getState().nodeInferenceMetrics
        expect(stored.get('node-a')?.totals).toEqual(reportedTotals)
        expect(stored.get('node-a')?.rates.meanTtftMs).toBe(100)
    })

    it('replaces the whole map on a push', () => {
        push({
            nodes: [
                {
                    nodeId: 'node-b',
                    totals: silentTotals,
                    rates: {
                        prefillTokensPerSecond: 0,
                        decodeTokensPerSecond: 0,
                        meanTtftMs: 0
                    },
                    models: []
                }
            ]
        })
        const stored = useMetricsStore.getState().nodeInferenceMetrics
        expect([...stored.keys()]).toEqual(['node-b'])
    })

    it('re-reads the snapshot on an interval so a decaying rate does not go stale', async () => {
        await vi.advanceTimersByTimeAsync(0)
        const before = getNodeMetrics.mock.calls.length
        await vi.advanceTimersByTimeAsync(5000)
        expect(getNodeMetrics.mock.calls.length).toBeGreaterThan(before)
    })

    it('keeps the last snapshot when a re-read fails', async () => {
        await vi.advanceTimersByTimeAsync(0)
        getNodeMetrics.mockRejectedValueOnce(new Error('broker is restarting'))
        await vi.advanceTimersByTimeAsync(5000)
        expect(useMetricsStore.getState().nodeInferenceMetrics.get('node-a')).toBeDefined()
    })

    it('re-reads the snapshot after a reset so the view is not left empty', async () => {
        await vi.advanceTimersByTimeAsync(0)
        useMetricsStore.getState().clearAll()
        await vi.advanceTimersByTimeAsync(0)
        expect(useMetricsStore.getState().nodeInferenceMetrics.get('node-a')).toBeDefined()
    })

    it('stops the re-read on cleanup', async () => {
        await vi.advanceTimersByTimeAsync(0)
        useMetricsStore.getState().cleanup()
        const before = getNodeMetrics.mock.calls.length
        await vi.advanceTimersByTimeAsync(60_000)
        expect(getNodeMetrics.mock.calls.length).toBe(before)
    })
})
