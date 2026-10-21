// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
    buildInferenceMetricsRows,
    buildModelMetricsRows,
    buildWorkloadMetricsRows,
    formatDurationMs,
    formatTokenCount,
    formatTokensPerSecond,
    hasDecodeMetrics,
    hasPhaseMetrics,
    hasPrefillMetrics,
    hasTtftMetrics,
    hasWorkloadMeasurement,
    phaseTokensPerSecond
} from '@/ui/utils/inference-metrics'
import type { InferenceMetricsTotals, NodeInferenceMetricsSnapshot } from '@/shared/types/metrics'
import type { Workload } from '@/shared/types/workloads'

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

describe('the presence counters', () => {
    it('tells a phase the engine never reported from one it did', () => {
        expect(hasPrefillMetrics(silentTotals)).toBe(false)
        expect(hasDecodeMetrics(silentTotals)).toBe(false)
        expect(hasTtftMetrics(silentTotals)).toBe(false)
    })

    it('counts a reported zero-length phase as reported', () => {
        expect(hasPrefillMetrics({ ...silentTotals, prefillRequests: 1 })).toBe(true)
        expect(hasDecodeMetrics({ ...silentTotals, decodeRequests: 1 })).toBe(true)
        expect(hasTtftMetrics({ ...silentTotals, ttftRequests: 1 })).toBe(true)
    })

    it('still counts a duration whose counter was lost', () => {
        expect(hasPrefillMetrics({ ...silentTotals, prefillMs: 10 })).toBe(true)
        expect(hasDecodeMetrics({ ...silentTotals, decodeMs: 10 })).toBe(true)
    })
})

describe('the node inference metrics rows', () => {
    it('marks an engine that reports no phase split as unavailable', () => {
        const rows = buildInferenceMetricsRows({
            nodeId: 'n1',
            totals: {
                requests: 3,
                prefillTokens: 100,
                decodeTokens: 20,
                cachedTokens: 0,
                prefillMs: 0,
                decodeMs: 0,
                ttftMs: 0,
                ttftRequests: 0,
                tokenRequests: 3,
                prefillRequests: 0,
                decodeRequests: 0
            },
            rates: { prefillTokensPerSecond: 0, decodeTokensPerSecond: 0, meanTtftMs: 0 },
            models: []
        })
        expect(rows.find(r => r.label === 'Prefill (last 60 s)')?.unavailable).toBe(true)
        expect(rows.find(r => r.label === 'Prefill (last 60 s)')?.value).toBe('not reported')
        expect(rows.find(r => r.label === 'Decode (last 60 s)')?.unavailable).toBe(true)
    })

    it('shows a measured phase', () => {
        const rows = buildInferenceMetricsRows({
            nodeId: 'n1',
            totals: {
                requests: 3,
                prefillTokens: 100,
                decodeTokens: 20,
                cachedTokens: 0,
                prefillMs: 1000,
                decodeMs: 2000,
                ttftMs: 300,
                ttftRequests: 3,
                tokenRequests: 3,
                prefillRequests: 3,
                decodeRequests: 3
            },
            rates: { prefillTokensPerSecond: 100, decodeTokensPerSecond: 10, meanTtftMs: 100 },
            models: []
        })
        expect(rows.find(r => r.label === 'Prefill (last 60 s)')?.unavailable).toBe(false)
        expect(rows.find(r => r.label === 'Prefill (last 60 s)')?.value).toBe('100 tok/s')
        expect(rows.find(r => r.label === 'Decode (last 60 s)')?.value).toBe('10 tok/s')
        expect(rows.find(r => r.label === 'TTFT (last 60 s)')?.value).toBe('100 ms')
    })

    it('gates the mean TTFT on the requests that reported one', () => {
        // The mean is zero when no request reported a TTFT, which is the same
        // number an engine reports for a first token that took no time. Only
        // the counter separates the two.
        const rows = buildInferenceMetricsRows({
            nodeId: 'n1',
            totals: { ...reportedTotals, ttftRequests: 0 },
            rates: { prefillTokensPerSecond: 100, decodeTokensPerSecond: 10, meanTtftMs: 0 },
            models: []
        })
        expect(rows.find(r => r.label === 'TTFT (last 60 s)')?.unavailable).toBe(true)
        expect(rows.find(r => r.label === 'TTFT (last 60 s)')?.value).toBe('not reported')
    })

    it('reports the tokens a node served', () => {
        const rows = buildInferenceMetricsRows({
            nodeId: 'n1',
            totals: reportedTotals,
            rates: { prefillTokensPerSecond: 100, decodeTokensPerSecond: 10, meanTtftMs: 100 },
            models: []
        })
        expect(
            rows.find(r => r.label === 'Prefill + decode tokens (since broker start)')?.value
        ).toBe('120')
        expect(
            rows.find(r => r.label === 'Prefill + decode tokens (since broker start)')?.unavailable
        ).toBe(false)
    })

    it('reports no tokens for an engine that reported no token count', () => {
        const rows = buildInferenceMetricsRows({
            nodeId: 'n1',
            totals: silentTotals,
            rates: { prefillTokensPerSecond: 0, decodeTokensPerSecond: 0, meanTtftMs: 0 },
            models: []
        })
        expect(
            rows.find(r => r.label === 'Prefill + decode tokens (since broker start)')?.unavailable
        ).toBe(true)
        expect(
            rows.find(r => r.label === 'Prefill + decode tokens (since broker start)')?.value
        ).toBe('not reported')
    })

    it('shows the cached tokens the node served', () => {
        // The cached count is the broker's own total for the node, shown
        // alongside the other totals. It appears once the node has served a
        // request that reported token counts at all.
        const rows = buildInferenceMetricsRows({
            nodeId: 'n1',
            totals: { ...reportedTotals, cachedTokens: 900 },
            rates: { prefillTokensPerSecond: 100, decodeTokensPerSecond: 10, meanTtftMs: 100 },
            models: []
        })
        expect(rows.find(r => r.label === 'Cached tokens (since broker start)')?.value).toBe('900')
        expect(rows.find(r => r.label === 'Cached tokens (since broker start)')?.unavailable).toBe(
            false
        )
    })

    it('omits the cached tokens for a node that served no measured request', () => {
        // A node whose engine reported no token count has no cached total to
        // show, so the row is left out rather than reading as a zero.
        const rows = buildInferenceMetricsRows({
            nodeId: 'n1',
            totals: silentTotals,
            rates: { prefillTokensPerSecond: 0, decodeTokensPerSecond: 0, meanTtftMs: 0 },
            models: []
        })
        expect(rows.find(r => r.label === 'Cached tokens (since broker start)')).toBeUndefined()
    })
})

describe('the per-model breakdown of a node', () => {
    it('rates a model by the tokens and the duration the broker reported for it', () => {
        const models = buildModelMetricsRows([{ model: 'llama-3.2-3b', totals: reportedTotals }])
        expect(models).toHaveLength(1)
        expect(models[0].model).toBe('llama-3.2-3b')
        expect(models[0].rows.find(r => r.label === 'Prefill (since broker start)')?.value).toBe(
            '100 tok/s'
        )
        expect(models[0].rows.find(r => r.label === 'Decode (since broker start)')?.value).toBe(
            '10 tok/s'
        )
        expect(
            models[0].rows.find(r => r.label === 'Prefill + decode tokens (since broker start)')
                ?.value
        ).toBe('120')
    })

    it('marks a model whose engine reported no phase as unavailable', () => {
        const models = buildModelMetricsRows([{ model: 'llama-3.2-3b', totals: silentTotals }])
        expect(
            models[0].rows.find(r => r.label === 'Prefill (since broker start)')?.unavailable
        ).toBe(true)
        expect(models[0].rows.find(r => r.label === 'Prefill (since broker start)')?.value).toBe(
            'not reported'
        )
        expect(
            models[0].rows.find(r => r.label === 'Prefill + decode tokens (since broker start)')
                ?.unavailable
        ).toBe(true)
    })
})

describe('a phase rate', () => {
    it('divides the tokens by the duration', () => {
        expect(phaseTokensPerSecond(1200, 340)).toBeCloseTo(3529.41, 2)
        expect(phaseTokensPerSecond(900, 2100)).toBeCloseTo(428.57, 2)
    })

    it('has no rate without a token count or a duration', () => {
        expect(phaseTokensPerSecond(null, 340)).toBeNull()
        expect(phaseTokensPerSecond(1200, null)).toBeNull()
        expect(phaseTokensPerSecond(undefined, undefined)).toBeNull()
    })

    it('never divides by a zero duration', () => {
        expect(phaseTokensPerSecond(1200, 0)).toBe(0)
    })
})

describe('the per-job measurement rows', () => {
    const measuredJob: Workload = {
        id: 'w1',
        model: 'llama-3.2-3b',
        engine: 'omlx',
        state: 'completed',
        originatedFrom: 'node-a',
        createdAt: 1,
        startedAt: 2,
        completedAt: 3,
        error: null,
        requesterId: null,
        prefillTokens: 1200,
        decodeTokens: 900,
        cachedTokens: 0,
        prefillMs: 340,
        decodeMs: 2100,
        ttftMs: 80,
        totalMs: 2440
    }

    it('shows the tokens, the phase durations and the rates', () => {
        const rows = buildWorkloadMetricsRows(measuredJob)
        expect(rows.find(r => r.label === 'Prefill (this request)')?.value).toBe(
            '1,200 tok · 340 ms · 3529 tok/s'
        )
        expect(rows.find(r => r.label === 'Decode (this request)')?.value).toBe(
            '900 tok · 2.1 s · 429 tok/s'
        )
        expect(rows.find(r => r.label === 'TTFT (this request)')?.value).toBe('80 ms')
        expect(rows.find(r => r.label === 'Total (this request)')?.value).toBe('2.4 s')
        expect(rows.every(r => !r.unavailable)).toBe(true)
    })

    it('reports the timing an engine did not report as not reported', () => {
        // An engine that reports token counts but no phase split (LM Studio)
        // leaves the duration out. It must not read as a zero duration, and it
        // must not carry a rate that no duration supports.
        const rows = buildWorkloadMetricsRows({
            ...measuredJob,
            prefillMs: null,
            decodeMs: null,
            ttftMs: null
        })
        expect(rows.find(r => r.label === 'Prefill (this request)')?.value).toBe(
            '1,200 tok · not reported'
        )
        expect(rows.find(r => r.label === 'Decode (this request)')?.value).toBe(
            '900 tok · not reported'
        )
        expect(rows.find(r => r.label === 'TTFT (this request)')?.value).toBe('not reported')
        expect(rows.find(r => r.label === 'TTFT (this request)')?.unavailable).toBe(true)
    })

    it('reports a measured zero as a zero', () => {
        const rows = buildWorkloadMetricsRows({
            ...measuredJob,
            decodeTokens: 0,
            decodeMs: 0
        })
        expect(rows.find(r => r.label === 'Decode (this request)')?.value).toBe(
            '0 tok · 0 ms · 0 tok/s'
        )
        expect(rows.find(r => r.label === 'Decode (this request)')?.unavailable).toBe(false)
    })

    it('omits the total for an engine that reports no total time', () => {
        // Ollama and LM Studio report no total time. The measurement carries
        // engine-reported values only, so there is no total to show and the
        // row is left out rather than filled with a duration the proxy
        // measured.
        const rows = buildWorkloadMetricsRows({ ...measuredJob, totalMs: null })
        expect(rows.find(r => r.label === 'Total (this request)')).toBeUndefined()
        // The phase rows stay: "this engine does not report phases" is worth
        // saying, while a missing total is simply not shown.
        expect(rows.find(r => r.label === 'Prefill (this request)')?.value).toBe(
            '1,200 tok · 340 ms · 3529 tok/s'
        )
        expect(rows.find(r => r.label === 'TTFT (this request)')?.value).toBe('80 ms')
    })

    it('shows the cached tokens the engine reported', () => {
        const rows = buildWorkloadMetricsRows({ ...measuredJob, cachedTokens: 90 })
        expect(rows.find(r => r.label === 'Cached tokens (this request)')?.value).toBe('90')
        expect(rows.find(r => r.label === 'Cached tokens (this request)')?.unavailable).toBe(false)
    })

    it('omits the cached tokens for an engine that reports none', () => {
        // Only oMLX reports a cached-token count, so for the other engines the
        // row is absent rather than a zero.
        const rows = buildWorkloadMetricsRows({ ...measuredJob, cachedTokens: null })
        expect(rows.find(r => r.label === 'Cached tokens (this request)')).toBeUndefined()
    })

    it('keeps a reported zero of cached tokens a zero', () => {
        // oMLX reports zero cached tokens on every request that missed the
        // prefix cache. That is a measurement the engine made.
        const rows = buildWorkloadMetricsRows(measuredJob)
        expect(rows.find(r => r.label === 'Cached tokens (this request)')?.value).toBe('0')
    })

    it('has no measurement for a job that reported nothing', () => {
        const running: Workload = {
            id: 'w3',
            model: 'llama-3.2-3b',
            engine: 'omlx',
            state: 'running',
            originatedFrom: 'node-a',
            createdAt: 1,
            startedAt: 2,
            completedAt: null,
            error: null,
            requesterId: null
        }
        const silent: Workload = {
            id: 'w2',
            model: 'llama-3.2-3b',
            engine: 'ollama',
            state: 'failed',
            originatedFrom: 'node-a',
            createdAt: 1,
            startedAt: 2,
            completedAt: 3,
            error: 'the engine refused the request',
            requesterId: null
        }
        expect(hasWorkloadMeasurement(running)).toBe(false)
        expect(hasWorkloadMeasurement(silent)).toBe(false)
        expect(hasWorkloadMeasurement(measuredJob)).toBe(true)
    })
})
