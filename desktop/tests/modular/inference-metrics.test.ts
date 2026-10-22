// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
    buildEngineNativeModels,
    buildEngineNativeRows,
    buildInferenceMetricsRows,
    buildModelMetricsRows,
    buildWorkloadLiveRows,
    buildWorkloadMetricsRows,
    engineNativeAgeMs,
    formatDurationMs,
    formatTokenCount,
    formatTokensPerSecond,
    hasDecodeMetrics,
    hasPhaseMetrics,
    hasPrefillMetrics,
    hasTtftMetrics,
    hasWorkloadMeasurement,
    isEngineNativeViewStale,
    phaseTokensPerSecond
} from '@/ui/utils/inference-metrics'
import type { InferenceMetricsTotals, NodeInferenceMetricsSnapshot } from '@/shared/types/metrics'
import type { Workload, WorkloadLiveProgress } from '@/shared/types/workloads'
import { liveProgressKey, mergeWorkloadLiveProgress, workloadKey } from '@/shared/utils/workloads'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import {
    parseEngineNativeView,
    parseNodeMetricsSnapshot,
    parseWorkloadLiveProgress,
    parseWorkloadsInitial
} from '@/electron/service-bridge/modular-state'
import { useMetricsStore } from '@/ui/stores/metrics.store'
import { useWorkloadsStore } from '@/ui/stores/workloads.store'

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

    it("reports a TTFT the broker's current window does not hold as not reported", () => {
        // The node measured a TTFT earlier — the lifetime counter says so —
        // but nothing was measured inside the broker's 60-second window, so
        // the windowed mean is zero. A zero mean must not read as a measured
        // "0 ms": a time-to-first-token is always positive, so a zero mean
        // means nothing was measured in this window.
        const rows = buildInferenceMetricsRows({
            nodeId: 'n1',
            totals: { ...reportedTotals, ttftRequests: 3 },
            rates: { prefillTokensPerSecond: 0, decodeTokensPerSecond: 0, meanTtftMs: 0 },
            models: []
        })
        expect(rows.find(r => r.label === 'TTFT (last 60 s)')?.unavailable).toBe(true)
        expect(rows.find(r => r.label === 'TTFT (last 60 s)')?.value).toBe('not reported')
    })

    it('keeps the TTFT row for a node that measured no TTFT at all', () => {
        // The row is always there, like the prefill and decode rows: what
        // the engine did not report is worth saying.
        const rows = buildInferenceMetricsRows({
            nodeId: 'n1',
            totals: silentTotals,
            rates: { prefillTokensPerSecond: 0, decodeTokensPerSecond: 0, meanTtftMs: 0 },
            models: []
        })
        expect(rows.find(r => r.label === 'TTFT (last 60 s)')).toBeDefined()
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

    it('shows no cached-token row for a node', () => {
        // Only oMLX reports a cached count, and the node's totals carry no
        // counter for the requests that reported one. A node-level total
        // would therefore read as a measured zero on a node whose engine
        // never measured a cached count, so the row is left out entirely.
        // The per-job row carries the count where the engine did report it.
        const rows = buildInferenceMetricsRows({
            nodeId: 'n1',
            totals: { ...reportedTotals, cachedTokens: 900 },
            rates: { prefillTokensPerSecond: 100, decodeTokensPerSecond: 10, meanTtftMs: 100 },
            models: []
        })
        expect(rows.find(r => r.label === 'Cached tokens (since broker start)')).toBeUndefined()
        expect(rows.some(r => r.label.startsWith('Cached tokens'))).toBe(false)
    })

    it('shows no cached-token row for a node that served no measured request', () => {
        // A node whose engine reported no token count has no cached total to
        // show either, so the row is left out rather than reading as a zero.
        const rows = buildInferenceMetricsRows({
            nodeId: 'n1',
            totals: silentTotals,
            rates: { prefillTokensPerSecond: 0, decodeTokensPerSecond: 0, meanTtftMs: 0 },
            models: []
        })
        expect(rows.some(r => r.label.startsWith('Cached tokens'))).toBe(false)
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
        // The per-model breakdown carries no cached row either: the same
        // absence the node's rows have.
        expect(models[0].rows.some(r => r.label.startsWith('Cached tokens'))).toBe(false)
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

describe("the engine's own view of itself", () => {
    const engineView = {
        engine: 'omlx',
        supported: true,
        totals: {
            requests: 2,
            promptTokens: 120,
            completionTokens: 34,
            cachedTokens: 90,
            cacheEfficiency: 75
        },
        rates: { prefillTokensPerSecond: 100, generationTokensPerSecond: 50 },
        models: [{ model: 'm', loaded: true }]
    }

    it("labels the engine self-report separately from the router's own numbers", () => {
        const rows = buildEngineNativeRows(engineView)
        expect(rows.length).toBeGreaterThan(0)
        expect(rows.every(r => r.source === 'engine')).toBe(true)
    })

    it('reports an engine with no native view as unsupported', () => {
        const rows = buildEngineNativeRows({ engine: 'ollama', supported: false })
        expect(rows).toEqual([])
    })

    it("labels the engine rows with the engine's own scope, never PAIR's", () => {
        // PAIR's own rows say "last 60 s" or "since broker start". The
        // engine's rows cover the engine's own life, so they must not answer
        // to either of those windows.
        const rows = buildEngineNativeRows(engineView)
        expect(rows.find(r => r.label === 'Requests (engine total)')?.value).toBe('2')
        expect(rows.find(r => r.label === 'Prompt tokens (engine total)')?.value).toBe('120')
        expect(rows.find(r => r.label === 'Completion tokens (engine total)')?.value).toBe('34')
        expect(rows.find(r => r.label === 'Cached tokens (engine total)')?.value).toBe('90')
        expect(rows.find(r => r.label === 'Cache efficiency (engine average)')?.value).toBe('75%')
        expect(rows.find(r => r.label === 'Prefill (engine average)')?.value).toBe('100 tok/s')
        expect(rows.find(r => r.label === 'Generation (engine average)')?.value).toBe('50 tok/s')
        expect(rows.some(r => r.label.includes('last 60 s'))).toBe(false)
        expect(rows.some(r => r.label.includes('since broker start'))).toBe(false)
    })

    it('leaves a value the engine never reported out of the rows', () => {
        // The engine publishes its counters with omitempty, so an absent key
        // means "not reported". A row that said "0" would read as a
        // measurement the engine never made.
        const rows = buildEngineNativeRows({
            engine: 'omlx',
            supported: true,
            totals: { requests: 2 },
            rates: {}
        })
        expect(rows.map(r => r.label)).toEqual(['Requests (engine total)'])
    })

    it('keeps a reported zero a zero', () => {
        const rows = buildEngineNativeRows({
            engine: 'omlx',
            supported: true,
            totals: { cachedTokens: 0 },
            rates: { generationTokensPerSecond: 0 }
        })
        expect(rows.find(r => r.label === 'Cached tokens (engine total)')?.value).toBe('0')
        expect(rows.find(r => r.label === 'Generation (engine average)')?.value).toBe('0 tok/s')
    })

    it('shows no rows for a view that reports no numbers at all', () => {
        // An empty totals object is not "the engine reported zeros".
        expect(buildEngineNativeRows({ engine: 'omlx', supported: true })).toEqual([])
        expect(buildEngineNativeRows({ engine: 'omlx', supported: true, totals: {} })).toEqual([])
    })

    it("keeps the engine's own models apart from the models loaded in memory", () => {
        // The engine's list says which models it offers; the engine-manager's
        // loaded set says which are resident in memory. The engine's own claim
        // is reported as "active", so the two never answer to the same word.
        const models = buildEngineNativeModels({
            engine: 'omlx',
            supported: true,
            models: [
                { model: 'mlx-community/Llama-3.2-3B-Instruct-4bit', loaded: true },
                { model: 'mlx-community/Qwen3-0.6B-4bit', loaded: false }
            ]
        })
        expect(models).toHaveLength(2)
        expect(models[0].active).toBe(true)
        expect(models[1].active).toBe(false)
    })

    it('lists no models for an engine with no native view', () => {
        expect(buildEngineNativeModels({ engine: 'ollama', supported: false })).toEqual([])
    })

    it('measures the age of the view, and says when it is stale', () => {
        const now = 1_716_998_400_000
        const fresh = { engine: 'omlx', supported: true, observedAt: now - 30_000 }
        expect(engineNativeAgeMs(fresh, now)).toBe(30_000)
        expect(isEngineNativeViewStale(fresh, now)).toBe(false)

        // The stamp freezes when the engine stops answering: the node keeps
        // its last good view, and the frozen stamp is how that shows.
        const dead = { engine: 'omlx', supported: true, observedAt: now - 600_000 }
        expect(engineNativeAgeMs(dead, now)).toBe(600_000)
        expect(isEngineNativeViewStale(dead, now)).toBe(true)
    })

    it('has no age to measure for a view with no stamp', () => {
        expect(engineNativeAgeMs({ engine: 'omlx', supported: true }, 1000)).toBeNull()
        expect(isEngineNativeViewStale(undefined, 1000)).toBe(false)
        expect(isEngineNativeViewStale({ engine: 'omlx', supported: true }, 1000)).toBe(false)
    })
})

describe('the engine-native view on the wire', () => {
    it("carries the engine's own counters off the node record", () => {
        const view = parseEngineNativeView({
            engine: 'omlx',
            supported: true,
            totals: {
                requests: 3270,
                promptTokens: 212978711,
                completionTokens: 2147906,
                cachedTokens: 157045860,
                cacheEfficiency: 73.7
            },
            rates: { prefillTokensPerSecond: 1553.2, generationTokensPerSecond: 32.5 },
            models: [{ model: 'mlx-community/Llama-3.2-3B-Instruct-4bit', loaded: true }],
            observedAt: 1716998400000
        })
        expect(view?.engine).toBe('omlx')
        expect(view?.supported).toBe(true)
        expect(view?.totals?.requests).toBe(3270)
        expect(view?.totals?.cacheEfficiency).toBe(73.7)
        expect(view?.rates?.generationTokensPerSecond).toBe(32.5)
        expect(view?.models?.[0].model).toBe('mlx-community/Llama-3.2-3B-Instruct-4bit')
        expect(view?.observedAt).toBe(1716998400000)
    })

    it('leaves a value the engine did not report absent, never zero', () => {
        const view = parseEngineNativeView({
            engine: 'omlx',
            supported: true,
            totals: { requests: 2 }
        })
        expect(view?.totals?.requests).toBe(2)
        expect(view?.totals?.promptTokens).toBeUndefined()
        expect(view?.rates).toBeUndefined()
        expect(view?.models).toBeUndefined()
    })

    it('drops a view that names no engine', () => {
        expect(parseEngineNativeView({ supported: true, totals: { requests: 2 } })).toBeUndefined()
        expect(parseEngineNativeView(undefined)).toBeUndefined()
        expect(parseEngineNativeView('nope')).toBeUndefined()
    })
})

describe('the live progress frame on the wire', () => {
    it("carries the origin and the origin proxy's own facts", () => {
        const parsed = parseWorkloadLiveProgress({
            jobId: 'w1',
            nodeId: 'node-b',
            originatedFrom: 'node-a',
            engine: 'omlx',
            runId: 'run-1',
            elapsedMs: 4200,
            chunks: 12,
            bytes: 2048,
            ttfbMs: 300,
            engineProgress: {
                requestId: 'eng-1',
                phase: 'generating',
                generatedTokens: 34,
                tokensPerSecond: 62.1,
                prefillProgress: 100,
                queuePosition: 0
            }
        })
        expect(parsed?.jobId).toBe('w1')
        // The origin is the half of the identity a job is keyed by: PAIR's
        // job id is a per-proxy counter, so two origins can both hold a
        // job "1".
        expect(parsed?.originatedFrom).toBe('node-a')
        expect(parsed?.nodeId).toBe('node-b')
        expect(parsed?.elapsedMs).toBe(4200)
        expect(parsed?.chunks).toBe(12)
        expect(parsed?.bytes).toBe(2048)
        expect(parsed?.ttfbMs).toBe(300)
        // The engine is part of the job's identity: every proxy counts its
        // own requests from 1, so an oMLX job "1" and an Ollama job "1" on
        // one node are two different jobs.
        expect(parsed?.engine).toBe('omlx')
        // The run nonce says which run the job id belongs to: a proxy that
        // restarts counts its jobs from 1 again, and the frame has to say
        // which run it is about.
        expect(parsed?.runId).toBe('run-1')
        expect(parsed?.engineProgress?.requestId).toBe('eng-1')
        expect(parsed?.engineProgress?.generatedTokens).toBe(34)
        expect(parsed?.engineProgress?.tokensPerSecond).toBe(62.1)
    })

    it('leaves a measurement nobody reported absent, never zero', () => {
        const parsed = parseWorkloadLiveProgress({
            jobId: 'w1',
            originatedFrom: 'node-a',
            engine: 'omlx'
        })
        expect(parsed?.elapsedMs).toBeNull()
        expect(parsed?.chunks).toBeNull()
        expect(parsed?.bytes).toBeNull()
        expect(parsed?.ttfbMs).toBeNull()
        // The engine published no live reading at all.
        expect(parsed?.engineProgress).toBeNull()
    })

    it('drops a frame that names no job', () => {
        // A frame without a job id would join no job's card.
        expect(parseWorkloadLiveProgress({ originatedFrom: 'node-a' })).toBeNull()
        expect(parseWorkloadLiveProgress(undefined)).toBeNull()
    })

    it('drops a frame that names no engine', () => {
        // The engine is part of the job's identity, so a frame without one
        // cannot be told apart from another engine's job with the same id.
        expect(parseWorkloadLiveProgress({ jobId: 'w1', originatedFrom: 'node-a' })).toBeNull()
    })
})

describe('the live progress of a running job', () => {
    /** The frame of a running oMLX request: PAIR's facts and the engine's reading. */
    const omlxLive: WorkloadLiveProgress = {
        jobId: 'w1',
        originatedFrom: 'node-a',
        engine: 'omlx',
        nodeId: 'node-b',
        elapsedMs: 4200,
        chunks: 12,
        bytes: 2048,
        ttfbMs: 300,
        engineProgress: {
            requestId: 'eng-1',
            phase: 'generating',
            generatedTokens: 34,
            tokensPerSecond: 62.1,
            prefillProgress: 100,
            queuePosition: 0
        }
    }

    it("shows the engine's own live token count and rate", () => {
        const rows = buildWorkloadLiveRows(omlxLive)
        expect(rows.find(r => r.label === 'Tokens (engine)')?.value).toBe('34')
        expect(rows.find(r => r.label === 'Rate (engine)')?.value).toBe('62 tok/s')
        expect(rows.find(r => r.label === 'Elapsed')?.value).toBe('4.2 s')
        // The time to the first byte is measured by PAIR and carried end to
        // end, so the live line shows it.
        expect(rows.find(r => r.label === 'First byte')?.value).toBe('300 ms')
        expect(rows.find(r => r.label === 'Received')?.value).toBe('12 chunks · 2,048 bytes')
    })

    it('never renders a chunk count as a token count', () => {
        const rows = buildWorkloadLiveRows(omlxLive)
        // The 12 chunks PAIR counted are chunks, not tokens: the only token
        // count on the line is the engine's own 34.
        expect(rows.find(r => r.label === 'Received')?.value).toBe('12 chunks · 2,048 bytes')
        expect(rows.find(r => r.label === 'Tokens (engine)')?.value).toBe('34')
        expect(rows.find(r => r.label === 'Tokens (engine)')?.value).not.toContain('12')
        // An engine that published no live reading reported no token count
        // either, so there is no token row at all.
        const silent = buildWorkloadLiveRows({ ...omlxLive, engineProgress: null })
        expect(silent.find(r => r.label === 'Tokens (engine)')).toBeUndefined()
    })

    it('says that an engine publishes no live progress instead of a row of gaps', () => {
        const rows = buildWorkloadLiveRows({ ...omlxLive, engineProgress: null })
        expect(rows.find(r => r.label === 'Elapsed')?.value).toBe('4.2 s')
        expect(rows.find(r => r.label === 'Received')?.value).toBe('12 chunks · 2,048 bytes')
        expect(rows.find(r => r.label === 'Engine live progress')?.value).toBe(
            'not reported (this engine publishes no live progress)'
        )
        expect(rows.find(r => r.label === 'Engine live progress')?.unavailable).toBe(true)
    })

    it('shows only the part of the received traffic that was measured', () => {
        const rows = buildWorkloadLiveRows({ ...omlxLive, bytes: null })
        expect(rows.find(r => r.label === 'Received')?.value).toBe('12 chunks')
    })

    it('reports a time to the first byte nobody measured as not reported', () => {
        const rows = buildWorkloadLiveRows({ ...omlxLive, ttfbMs: null })
        const row = rows.find(r => r.label === 'First byte')
        expect(row?.value).toBe('not reported')
        expect(row?.unavailable).toBe(true)
    })

    it('reports a reading the engine did not make as not reported, never as zero', () => {
        const rows = buildWorkloadLiveRows({
            ...omlxLive,
            engineProgress: {
                requestId: 'eng-1',
                phase: 'waiting',
                generatedTokens: null,
                tokensPerSecond: null,
                prefillProgress: null,
                queuePosition: null
            }
        })
        expect(rows.find(r => r.label === 'Tokens (engine)')?.value).toBe('not reported')
        expect(rows.find(r => r.label === 'Rate (engine)')?.value).toBe('not reported')
        expect(rows.find(r => r.label === 'Prefill progress (engine)')?.value).toBe('not reported')
        expect(rows.find(r => r.label === 'Queue position (engine)')?.value).toBe('not reported')
        expect(rows.find(r => r.label === 'Tokens (engine)')?.unavailable).toBe(true)
    })
})

describe('merging a live progress frame', () => {
    /** What PAIR measured of the client's stream. */
    const facts: WorkloadLiveProgress = {
        jobId: 'w1',
        originatedFrom: 'node-a',
        engine: 'omlx',
        nodeId: 'node-b',
        elapsedMs: 4200,
        chunks: 12,
        bytes: 2048,
        ttfbMs: 300,
        engineProgress: null
    }

    /** What the serving node's engine reported of its own request. */
    const engineOnly: WorkloadLiveProgress = {
        jobId: 'w1',
        originatedFrom: 'node-a',
        engine: 'omlx',
        nodeId: 'node-b',
        engineProgress: {
            requestId: 'eng-1',
            phase: 'generating',
            generatedTokens: 34,
            tokensPerSecond: 62.1,
            prefillProgress: 100,
            queuePosition: 0
        }
    }

    it('keeps the engine reading when the next frame carries only the PAIR facts', () => {
        const merged = mergeWorkloadLiveProgress(engineOnly, facts)
        expect(merged.engineProgress?.generatedTokens).toBe(34)
        expect(merged.elapsedMs).toBe(4200)
        expect(merged.chunks).toBe(12)
        expect(merged.bytes).toBe(2048)
    })

    it('keeps the PAIR facts when the next frame carries only the engine reading', () => {
        const merged = mergeWorkloadLiveProgress(facts, engineOnly)
        expect(merged.elapsedMs).toBe(4200)
        expect(merged.chunks).toBe(12)
        expect(merged.bytes).toBe(2048)
        expect(merged.engineProgress?.generatedTokens).toBe(34)
    })

    it('overwrites the value a frame does carry', () => {
        const merged = mergeWorkloadLiveProgress(facts, { ...facts, elapsedMs: 5000 })
        expect(merged.elapsedMs).toBe(5000)
    })

    it('replaces the view when the frame names another run of the proxy', () => {
        // A proxy that restarts counts its jobs from 1 again, so the job id
        // "w1" of the new run is a DIFFERENT job from the job "w1" of the old
        // one. The stored view belongs to the job it replaced: it is
        // replaced, not merged, or the old job's engine reading would be
        // shown on the new job's card. The broker merges the same way.
        const oldRun: WorkloadLiveProgress = {
            ...facts,
            runId: 'run-1',
            engineProgress: {
                requestId: 'eng-1',
                phase: 'generating',
                generatedTokens: 34,
                tokensPerSecond: 62.1,
                prefillProgress: 100,
                queuePosition: 0
            }
        }
        const newRun: WorkloadLiveProgress = {
            jobId: 'w1',
            originatedFrom: 'node-a',
            engine: 'omlx',
            runId: 'run-2',
            elapsedMs: 1200
        }
        const merged = mergeWorkloadLiveProgress(oldRun, newRun)
        expect(merged.runId).toBe('run-2')
        expect(merged.elapsedMs).toBe(1200)
        // The old run's engine reading is gone, not carried over.
        expect(merged.engineProgress).toBeNull()
        expect(merged.chunks).toBeNull()
        expect(merged.bytes).toBeNull()
        expect(merged.ttfbMs).toBeNull()
    })

    it('keeps the view when the frame names the same run of the proxy', () => {
        const stored: WorkloadLiveProgress = { ...facts, runId: 'run-1' }
        const merged = mergeWorkloadLiveProgress(stored, {
            ...facts,
            runId: 'run-1',
            elapsedMs: 5000
        })
        expect(merged.runId).toBe('run-1')
        expect(merged.elapsedMs).toBe(5000)
        expect(merged.chunks).toBe(12)
    })

    it('replaces the engine reading as a unit', () => {
        // A later engine row supersedes the earlier one: it does not blend
        // with it, and a field the engine has stopped reporting stops being
        // shown rather than falling back to the earlier reading.
        const later: WorkloadLiveProgress = {
            ...engineOnly,
            engineProgress: {
                requestId: 'eng-2',
                phase: 'generating',
                generatedTokens: 90,
                tokensPerSecond: null,
                prefillProgress: null,
                queuePosition: null
            }
        }
        const merged = mergeWorkloadLiveProgress(engineOnly, later)
        expect(merged.engineProgress?.requestId).toBe('eng-2')
        expect(merged.engineProgress?.generatedTokens).toBe(90)
        expect(merged.engineProgress?.tokensPerSecond).toBeNull()
    })

    it('never turns an absent measurement into a zero', () => {
        const merged = mergeWorkloadLiveProgress(undefined, {
            jobId: 'w1',
            originatedFrom: 'node-a',
            engine: 'omlx'
        })
        expect(merged.elapsedMs).toBeNull()
        expect(merged.chunks).toBeNull()
        expect(merged.bytes).toBeNull()
        expect(merged.engineProgress).toBeNull()
    })
})

describe('the workloads store keeps the live view of a running job', () => {
    let pushProgress: (frame: WorkloadLiveProgress) => void = () => {}
    let pushUpsert: (workload: Workload) => void = () => {}
    let scheduled: (() => void) | null = null

    beforeEach(() => {
        scheduled = null
        // The store buffers pushes and flushes them on the next animation
        // frame, so the test drives the frame itself.
        vi.stubGlobal('requestAnimationFrame', (callback: () => void): number => {
            scheduled = callback
            return 1
        })
        vi.stubGlobal('cancelAnimationFrame', () => {})
        vi.stubGlobal('window', {
            pairApi: {
                workloads: {
                    getInitial: async () => ({}),
                    onUpsert: (callback: (workload: Workload) => void) => {
                        pushUpsert = callback
                        return () => {
                            pushUpsert = () => {}
                        }
                    },
                    onRemove: () => () => {},
                    onProgress: (callback: (frame: WorkloadLiveProgress) => void) => {
                        pushProgress = callback
                        return () => {
                            pushProgress = () => {}
                        }
                    }
                }
            }
        })
    })

    afterEach(() => {
        useWorkloadsStore.getState().cleanup()
        vi.unstubAllGlobals()
    })

    /** Run the store's pending flush, as the next animation frame would. */
    function flush() {
        if (!scheduled) throw new Error('the store scheduled no flush')
        const pending = scheduled
        scheduled = null
        pending()
    }

    async function initialize() {
        await useWorkloadsStore.getState().initialize()
    }

    const runningJob: Workload = {
        id: 'w1',
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

    it('keeps a live frame out of the catalog', async () => {
        await initialize()
        pushProgress({
            jobId: 'w1',
            originatedFrom: 'node-a',
            engine: 'omlx',
            elapsedMs: 4200,
            chunks: 12,
            bytes: 2048
        })
        flush()
        const state = useWorkloadsStore.getState()
        expect(state.liveProgress.get(liveProgressKey('node-a', 'omlx', 'w1'))?.elapsedMs).toBe(
            4200
        )
        // A live frame is a snapshot of a running request, not a catalog
        // event: it never enters the catalog.
        expect(state.workloads.has(workloadKey('node-a', 'w1'))).toBe(false)
    })

    it('keys the live view by the origin and the job id', async () => {
        await initialize()
        // PAIR's job id is a per-proxy counter, so two origins can both hold
        // a job "1" — and only the origin tells their jobs apart.
        pushProgress({ jobId: '1', originatedFrom: 'origin-a', engine: 'omlx', elapsedMs: 4200 })
        pushProgress({ jobId: '1', originatedFrom: 'origin-c', engine: 'omlx', elapsedMs: 4300 })
        flush()
        const live = useWorkloadsStore.getState().liveProgress
        expect(live.get(liveProgressKey('origin-a', 'omlx', '1'))?.elapsedMs).toBe(4200)
        expect(live.get(liveProgressKey('origin-c', 'omlx', '1'))?.elapsedMs).toBe(4300)
    })

    it('keys the live view by the engine as well', async () => {
        await initialize()
        // Every proxy counts its own requests from 1, so an oMLX job "1" and
        // an Ollama job "1" on the SAME node are two different jobs. The
        // engine is what tells them apart: without it, the last frame
        // would overwrite the other's counts and one job's card would show
        // the other job's numbers.
        pushProgress({ jobId: '1', originatedFrom: 'node-a', engine: 'omlx', elapsedMs: 4200 })
        pushProgress({ jobId: '1', originatedFrom: 'node-a', engine: 'ollama', elapsedMs: 4300 })
        flush()
        const live = useWorkloadsStore.getState().liveProgress
        expect(live.get(liveProgressKey('node-a', 'omlx', '1'))?.elapsedMs).toBe(4200)
        expect(live.get(liveProgressKey('node-a', 'ollama', '1'))?.elapsedMs).toBe(4300)
    })

    it('keeps what an earlier frame supplied when the next frame is partial', async () => {
        await initialize()
        pushProgress({
            jobId: 'w1',
            originatedFrom: 'node-a',
            engine: 'omlx',
            elapsedMs: 4200,
            chunks: 12,
            bytes: 2048
        })
        pushProgress({
            jobId: 'w1',
            originatedFrom: 'node-a',
            engine: 'omlx',
            engineProgress: {
                requestId: 'eng-1',
                phase: 'generating',
                generatedTokens: 34,
                tokensPerSecond: 62.1
            }
        })
        flush()
        const view = useWorkloadsStore
            .getState()
            .liveProgress.get(liveProgressKey('node-a', 'omlx', 'w1'))
        expect(view?.elapsedMs).toBe(4200)
        expect(view?.chunks).toBe(12)
        expect(view?.bytes).toBe(2048)
        expect(view?.engineProgress?.generatedTokens).toBe(34)
    })

    it('keeps the engine reading when a later frame carries only the PAIR facts', async () => {
        await initialize()
        pushProgress({
            jobId: 'w1',
            originatedFrom: 'node-a',
            engine: 'omlx',
            engineProgress: {
                requestId: 'eng-1',
                phase: 'generating',
                generatedTokens: 34,
                tokensPerSecond: 62.1
            }
        })
        pushProgress({ jobId: 'w1', originatedFrom: 'node-a', engine: 'omlx', elapsedMs: 4200 })
        flush()
        const view = useWorkloadsStore
            .getState()
            .liveProgress.get(liveProgressKey('node-a', 'omlx', 'w1'))
        expect(view?.elapsedMs).toBe(4200)
        expect(view?.engineProgress?.generatedTokens).toBe(34)
    })

    it('retires the live view of a finished job and never resurrects it', async () => {
        await initialize()
        pushUpsert(runningJob)
        pushProgress({ jobId: 'w1', originatedFrom: 'node-a', engine: 'omlx', elapsedMs: 4200 })
        flush()
        expect(
            useWorkloadsStore.getState().liveProgress.has(liveProgressKey('node-a', 'omlx', 'w1'))
        ).toBe(true)

        pushUpsert({ ...runningJob, state: 'completed', completedAt: 3 })
        flush()
        expect(
            useWorkloadsStore.getState().liveProgress.has(liveProgressKey('node-a', 'omlx', 'w1'))
        ).toBe(false)

        // A frame that was already on its way must not bring the job back.
        pushProgress({ jobId: 'w1', originatedFrom: 'node-a', engine: 'omlx', elapsedMs: 4400 })
        flush()
        expect(
            useWorkloadsStore.getState().liveProgress.has(liveProgressKey('node-a', 'omlx', 'w1'))
        ).toBe(false)
    })
})
