// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// @vitest-environment jsdom

import { act, createElement, type ReactNode } from 'react'
import { createRoot } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type {
    EngineNativeView,
    InferenceMetricsTotals,
    NodeInferenceMetrics
} from '@/shared/types/metrics'
import type { NodeItem } from '@/shared/types/nodes'
import type { Workload, WorkloadLiveProgress } from '@/shared/types/workloads'
import { liveProgressKey, mergeWorkloadLiveProgress } from '@/shared/utils/workloads'
import { useMetricsStore } from '@/ui/stores/metrics.store'
import { useNodesStore } from '@/ui/stores/nodes.store'
import { useWorkloadsStore } from '@/ui/stores/workloads.store'
import NodeCardDetails from '@/ui/components/NodeList/NodeCardDetails'
import NodeInferenceMetricsSection from '@/ui/components/NodeList/NodeInferenceMetricsSection'
import WorkloadItemCard from '@/ui/components/Workloads/WorkloadItemCard'

let container: HTMLDivElement
let root: ReturnType<typeof createRoot>

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
    prefillRequests: 3,
    decodeRequests: 3
}

/** A node whose engine reported no phase at all, only token counts. */
const tokenOnlyTotals: InferenceMetricsTotals = {
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
}

function nodeMetrics(totals: InferenceMetricsTotals): NodeInferenceMetrics {
    return {
        nodeId: 'node-a',
        totals,
        rates: {
            prefillTokensPerSecond: 100,
            decodeTokensPerSecond: 10,
            // The broker's mean TTFT is zero when no request inside its
            // window reported a TTFT, so the fixture follows the totals.
            meanTtftMs: totals.ttftRequests > 0 ? 100 : 0
        },
        models: [{ model: 'llama-3.2-3b', totals }]
    }
}

async function render(content: ReactNode) {
    await act(async () => root.render(content))
}

function text() {
    const walker = document.createTreeWalker(container, NodeFilter.SHOW_TEXT)
    const parts: string[] = []
    while (walker.nextNode()) parts.push(walker.currentNode.textContent ?? '')
    return parts.join(' ').replace(/\s+/g, ' ').trim()
}

async function click(element: Element) {
    await act(async () => {
        element.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    })
}

beforeEach(() => {
    vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true)
    vi.stubGlobal(
        'ResizeObserver',
        class {
            observe = vi.fn()
            disconnect = vi.fn()
        }
    )
    vi.stubGlobal('requestAnimationFrame', () => 1)
    vi.stubGlobal('cancelAnimationFrame', () => {})
    container = document.createElement('div')
    document.body.append(container)
    root = createRoot(container)
})

afterEach(async () => {
    await act(async () => root.unmount())
    container.remove()
    useMetricsStore.getState().clearAll()
    useNodesStore.setState({ nodes: new Map() })
    useWorkloadsStore.setState({ liveProgress: new Map() })
    vi.unstubAllGlobals()
})

describe('the node card prefill/decode section', () => {
    it('is rendered by the node card itself', async () => {
        const node: NodeItem = {
            id: 'node-a',
            name: 'Fixture node',
            ipAddress: '192.0.2.1',
            port: 14318,
            allIpAddresses: [],
            status: 'active',
            os: 'Linux',
            topology: {
                gpus: [],
                cpu: { model: 'Fixture CPU', cores: 4, threads: 4 },
                ram: 16 * 1024 ** 3,
                storage: []
            }
        }
        useMetricsStore.setState({
            nodeInferenceMetrics: new Map([['node-a', nodeMetrics(reportedTotals)]])
        })
        await render(createElement(NodeCardDetails, { node }))
        expect(text()).toContain('Prefill (last 60 s) 100 tok/s')
        expect(text()).toContain('Prefill + decode tokens (since broker start) 120')
    })

    it('renders the rates, the mean TTFT and the tokens the node served', async () => {
        useMetricsStore.setState({
            nodeInferenceMetrics: new Map([['node-a', nodeMetrics(reportedTotals)]])
        })
        await render(createElement(NodeInferenceMetricsSection, { nodeId: 'node-a' }))
        expect(text()).toContain('Prefill (last 60 s) 100 tok/s')
        expect(text()).toContain('Decode (last 60 s) 10 tok/s')
        expect(text()).toContain('TTFT (last 60 s) 100 ms')
        expect(text()).toContain('Prefill + decode tokens (since broker start) 120')
    })

    it('renders no cached-token row for a node', async () => {
        // Only oMLX reports a cached count, and the node's totals carry no
        // counter for the requests that reported one, so a node-level total
        // would read as a measured zero on a node whose engine never
        // measured one. The row is left out; the job card carries the count.
        useMetricsStore.setState({
            nodeInferenceMetrics: new Map([
                ['node-a', nodeMetrics({ ...reportedTotals, cachedTokens: 900 })]
            ])
        })
        await render(createElement(NodeInferenceMetricsSection, { nodeId: 'node-a' }))
        expect(text()).not.toContain('Cached tokens')
    })

    it('renders no cached-token row for a node that served no measured request', async () => {
        useMetricsStore.setState({
            nodeInferenceMetrics: new Map([
                [
                    'node-a',
                    nodeMetrics({
                        ...tokenOnlyTotals,
                        tokenRequests: 0,
                        prefillTokens: 0,
                        decodeTokens: 0
                    })
                ]
            ])
        })
        await render(createElement(NodeInferenceMetricsSection, { nodeId: 'node-a' }))
        expect(text()).not.toContain('Cached tokens')
    })

    it('labels the node rates with the window they cover', async () => {
        // The node's rates are the broker's 60-second window; the per-model
        // ratios are lifetime figures. The two must never answer to the same
        // label, or a model that has gone idle reads as contradictory.
        useMetricsStore.setState({
            nodeInferenceMetrics: new Map([['node-a', nodeMetrics(reportedTotals)]])
        })
        await render(createElement(NodeInferenceMetricsSection, { nodeId: 'node-a' }))
        expect(text()).toContain('Prefill (last 60 s)')
        expect(text()).not.toContain('Prefill (since broker start)')

        const toggle = container.querySelector('button')
        if (toggle === null) throw new Error('expected a per-model toggle')
        await click(toggle)
        expect(text()).toContain('Prefill (since broker start)')
        expect(text()).toContain('Decode (since broker start)')
    })

    it('renders a phase the engine never reported as not reported', async () => {
        useMetricsStore.setState({
            nodeInferenceMetrics: new Map([['node-a', nodeMetrics(tokenOnlyTotals)]])
        })
        await render(createElement(NodeInferenceMetricsSection, { nodeId: 'node-a' }))
        expect(text()).toContain('Prefill (last 60 s) not reported')
        expect(text()).toContain('Decode (last 60 s) not reported')
        expect(text()).toContain('TTFT (last 60 s) not reported')
        // The token counts were reported, so they are shown.
        expect(text()).toContain('Prefill + decode tokens (since broker start) 120')
        expect(text()).not.toContain('0 tok/s')
        expect(text()).not.toContain('0 ms')
    })

    it('renders a node whose window holds no measured TTFT as not reported', async () => {
        // The node measured a TTFT earlier — the lifetime counter says so —
        // but the broker's current window holds none, so the windowed mean
        // is zero. It must not read as a measured "0 ms".
        useMetricsStore.setState({
            nodeInferenceMetrics: new Map([['node-a', nodeMetrics(reportedTotals)]])
        })
        await render(createElement(NodeInferenceMetricsSection, { nodeId: 'node-a' }))
        expect(text()).toContain('TTFT (last 60 s) 100 ms')

        const idle: NodeInferenceMetrics = {
            ...nodeMetrics(reportedTotals),
            rates: { prefillTokensPerSecond: 0, decodeTokensPerSecond: 0, meanTtftMs: 0 }
        }
        useMetricsStore.setState({ nodeInferenceMetrics: new Map([['node-a', idle]]) })
        await render(createElement(NodeInferenceMetricsSection, { nodeId: 'node-a' }))
        expect(text()).toContain('TTFT (last 60 s) not reported')
        expect(text()).not.toContain('0 ms')
    })

    it('keeps the per-model breakdown collapsed until it is asked for', async () => {
        useMetricsStore.setState({
            nodeInferenceMetrics: new Map([['node-a', nodeMetrics(reportedTotals)]])
        })
        await render(createElement(NodeInferenceMetricsSection, { nodeId: 'node-a' }))
        expect(text()).not.toContain('llama-3.2-3b')

        const toggle = container.querySelector('button')
        if (toggle === null) throw new Error('expected a per-model toggle')
        await click(toggle)
        expect(text()).toContain('llama-3.2-3b')
        expect(text()).toContain('Prefill (since broker start) 100 tok/s')
    })

    it('renders nothing for a node that never served anything', async () => {
        useMetricsStore.setState({ nodeInferenceMetrics: new Map() })
        await render(createElement(NodeInferenceMetricsSection, { nodeId: 'node-a' }))
        expect(text()).toBe('')
    })
})

describe('the job card measurement', () => {
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
        prefillMs: 340,
        decodeMs: 2100,
        ttftMs: 80,
        totalMs: 2440
    }

    it('renders the tokens, the phase durations and the rates of a finished job', async () => {
        await render(createElement(WorkloadItemCard, { workload: measuredJob }))
        expect(text()).toContain('Prefill (this request) 1,200 tok · 340 ms · 3529 tok/s')
        expect(text()).toContain('Decode (this request) 900 tok · 2.1 s · 429 tok/s')
        expect(text()).toContain('TTFT (this request) 80 ms')
        expect(text()).toContain('Total (this request) 2.4 s')
    })

    it('renders the cached tokens the engine reported', async () => {
        await render(
            createElement(WorkloadItemCard, { workload: { ...measuredJob, cachedTokens: 90 } })
        )
        expect(text()).toContain('Cached tokens (this request) 90')
    })

    it('renders no cached tokens for an engine that reports none', async () => {
        await render(
            createElement(WorkloadItemCard, { workload: { ...measuredJob, cachedTokens: null } })
        )
        expect(text()).not.toContain('Cached tokens')
    })

    it('renders no total for an engine that reports no total time', async () => {
        // The row is left out entirely: an engine that reports no total time
        // has no total to show, and the proxy's own duration must not stand
        // in for it.
        await render(
            createElement(WorkloadItemCard, { workload: { ...measuredJob, totalMs: null } })
        )
        expect(text()).not.toContain('Total (this request)')
        expect(text()).toContain('TTFT (this request) 80 ms')
    })

    it('renders the timing an engine did not report as not reported', async () => {
        await render(
            createElement(WorkloadItemCard, {
                workload: { ...measuredJob, prefillMs: null, decodeMs: null, ttftMs: null }
            })
        )
        expect(text()).toContain('Prefill (this request) 1,200 tok · not reported')
        expect(text()).toContain('Decode (this request) 900 tok · not reported')
        expect(text()).toContain('TTFT (this request) not reported')
        expect(text()).not.toContain('0 ms')
    })

    it('renders no measurement for a running job', async () => {
        // A running job has no measurement at all: the numbers exist only
        // once the engine has answered. The fields are kept here anyway, so
        // the test proves the state gate and not only the missing fields.
        await render(
            createElement(WorkloadItemCard, {
                workload: { ...measuredJob, state: 'running', completedAt: null }
            })
        )
        expect(text()).not.toContain('Prefill')
        expect(text()).not.toContain('tok/s')
    })
})

describe('the live line of a running job', () => {
    const runningOmlxJob: Workload = {
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

    const runningOllamaJob: Workload = {
        ...runningOmlxJob,
        id: 'w2',
        engine: 'ollama'
    }

    /**
     * The frame of a running oMLX request: what PAIR measured of the
     * client's stream, plus the engine's own live reading.
     */
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

    /**
     * The frame of a request whose engine publishes no live progress
     * (Ollama, LM Studio): PAIR's own facts, and nothing from an engine.
     */
    const ollamaLive: WorkloadLiveProgress = {
        jobId: 'w2',
        originatedFrom: 'node-a',
        engine: 'ollama',
        nodeId: 'node-a',
        elapsedMs: 4200,
        chunks: 12,
        bytes: 2048,
        ttfbMs: 300,
        engineProgress: null
    }

    /** The live view of one job, as the store holds it. */
    function setLiveProgress(view: WorkloadLiveProgress) {
        act(() => {
            useWorkloadsStore.setState({
                liveProgress: new Map([
                    [liveProgressKey(view.originatedFrom, view.engine, view.jobId), view]
                ])
            })
        })
    }

    it("shows the engine's live token count and rate on a running oMLX job", async () => {
        setLiveProgress(omlxLive)
        await render(createElement(WorkloadItemCard, { workload: runningOmlxJob }))
        const rendered = text()
        expect(rendered).toContain('Tokens (engine) 34')
        expect(rendered).toContain('Rate (engine) 62 tok/s')
        // What PAIR measured of the client's stream is on the same line.
        expect(rendered).toContain('Elapsed 4.2 s')
        expect(rendered).toContain('Received 12 chunks · 2,048 bytes')
    })

    it('shows the elapsed time and the received bytes for an engine with no live progress', async () => {
        setLiveProgress(ollamaLive)
        await render(createElement(WorkloadItemCard, { workload: runningOllamaJob }))
        const rendered = text()
        expect(rendered).toContain('Elapsed 4.2 s')
        expect(rendered).toContain('Received 12 chunks · 2,048 bytes')
        // The engine published no live reading at all, and the card says so
        // rather than showing a row of "not reported" for every engine field.
        expect(rendered).toContain(
            'Engine live progress not reported (this engine publishes no live progress)'
        )
    })

    it('never renders a chunk count as a token count', async () => {
        setLiveProgress(omlxLive)
        await render(createElement(WorkloadItemCard, { workload: runningOmlxJob }))
        let rendered = text()
        // The only token count on the card is the engine's own 34. The 12
        // chunks PAIR counted are chunks, not tokens.
        expect(rendered).toContain('Tokens (engine) 34')
        expect(rendered).not.toContain('Tokens (engine) 12')
        expect(rendered).not.toContain('12 tok')
        expect(rendered).not.toContain('12 tokens')

        // An engine that publishes no live progress reported no token count
        // either, so the card shows no token count at all.
        setLiveProgress(ollamaLive)
        await render(createElement(WorkloadItemCard, { workload: runningOllamaJob }))
        rendered = text()
        expect(rendered).not.toContain('Tokens')
        expect(rendered).not.toContain('tok')
    })

    it('shows each job its own live view when two engines number their jobs alike', async () => {
        // Every proxy counts its own requests from 1, so an oMLX job "1" and
        // an Ollama job "1" on the same node are two different jobs. Each
        // card must show its own job's live numbers — the oMLX card the
        // engine's token count, the Ollama card its own chunks.
        const omlxJob: Workload = { ...runningOmlxJob, id: '1' }
        const ollamaJob: Workload = { ...runningOllamaJob, id: '1' }
        act(() => {
            useWorkloadsStore.setState({
                liveProgress: new Map([
                    [liveProgressKey('node-a', 'omlx', '1'), { ...omlxLive, jobId: '1' }],
                    [
                        liveProgressKey('node-a', 'ollama', '1'),
                        { ...ollamaLive, jobId: '1', chunks: 30 }
                    ]
                ])
            })
        })

        await render(createElement(WorkloadItemCard, { workload: omlxJob }))
        let rendered = text()
        expect(rendered).toContain('Tokens (engine) 34')
        expect(rendered).toContain('Received 12 chunks · 2,048 bytes')

        await render(createElement(WorkloadItemCard, { workload: ollamaJob }))
        rendered = text()
        // The Ollama job's own numbers, and none of the oMLX job's.
        expect(rendered).toContain('Received 30 chunks · 2,048 bytes')
        expect(rendered).not.toContain('Tokens (engine)')
    })

    it('shows the final numbers of a finished job and no live line', async () => {
        const finishedJob: Workload = {
            id: 'w3',
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
            cachedTokens: 90,
            prefillMs: 340,
            decodeMs: 2100,
            ttftMs: 80,
            totalMs: 2440
        }
        // The broker retires a finished job's live view, but a frame that is
        // still in flight must not put a live line on a finished card: the
        // final numbers are the ones that count.
        setLiveProgress({ ...omlxLive, jobId: 'w3' })
        await render(createElement(WorkloadItemCard, { workload: finishedJob }))
        const rendered = text()
        expect(rendered).toContain('Prefill (this request) 1,200 tok · 340 ms · 3529 tok/s')
        expect(rendered).toContain('Decode (this request) 900 tok · 2.1 s · 429 tok/s')
        expect(rendered).toContain('TTFT (this request) 80 ms')
        expect(rendered).toContain('Total (this request) 2.4 s')
        expect(rendered).toContain('Cached tokens (this request) 90')
        // No live line at all.
        expect(rendered).not.toContain('Elapsed')
        expect(rendered).not.toContain('Received')
        expect(rendered).not.toContain('Engine live progress')
        expect(rendered).not.toContain('Tokens (engine)')
        expect(rendered).not.toContain('Rate (engine)')
        expect(rendered).not.toContain('Prefill progress (engine)')
        expect(rendered).not.toContain('Queue position (engine)')
    })

    it('keeps what an earlier frame supplied when the next frame is partial', async () => {
        // The origin proxy publishes what PAIR measured of the client's
        // stream; the serving node publishes the engine's reading. Neither
        // sees the other's facts, so a reader that merged them naively would
        // blank the elapsed time the moment the engine's frame arrived.
        const facts: WorkloadLiveProgress = {
            jobId: 'w1',
            originatedFrom: 'node-a',
            engine: 'omlx',
            nodeId: 'node-b',
            elapsedMs: 4200,
            chunks: 12,
            bytes: 2048,
            engineProgress: null
        }
        const engineOnly: WorkloadLiveProgress = {
            jobId: 'w1',
            originatedFrom: 'node-a',
            engine: 'omlx',
            nodeId: 'node-b',
            engineProgress: omlxLive.engineProgress
        }

        // The engine's frame arrives second and carries no PAIR facts.
        setLiveProgress(mergeWorkloadLiveProgress(facts, engineOnly))
        await render(createElement(WorkloadItemCard, { workload: runningOmlxJob }))
        let rendered = text()
        expect(rendered).toContain('Elapsed 4.2 s')
        expect(rendered).toContain('Received 12 chunks · 2,048 bytes')
        expect(rendered).toContain('Tokens (engine) 34')

        // And the other way round: the PAIR facts arrive second and carry no
        // engine reading, which the reader must not blank either.
        setLiveProgress(mergeWorkloadLiveProgress(engineOnly, facts))
        await render(createElement(WorkloadItemCard, { workload: runningOmlxJob }))
        rendered = text()
        expect(rendered).toContain('Elapsed 4.2 s')
        expect(rendered).toContain('Received 12 chunks · 2,048 bytes')
        expect(rendered).toContain('Tokens (engine) 34')
    })
})

describe("the node card's engine-native section", () => {
    /** A node whose engine publishes a view of itself. */
    function nodeWithEngineNative(engineNative: EngineNativeView): NodeItem {
        const node: NodeItem = {
            id: 'node-a',
            name: 'Fixture node',
            ipAddress: '192.0.2.1',
            port: 14318,
            allIpAddresses: [],
            status: 'active',
            os: 'Linux',
            topology: {
                gpus: [],
                cpu: { model: 'Fixture CPU', cores: 4, threads: 4 },
                ram: 16 * 1024 ** 3,
                storage: []
            },
            engineNative
        }
        act(() => {
            useNodesStore.setState({ nodes: new Map([['node-a', node]]) })
        })
        return node
    }

    it("heads the engine's own counters and says what they are", async () => {
        nodeWithEngineNative({
            engine: 'omlx',
            supported: true,
            totals: { requests: 3270, promptTokens: 212978711, cachedTokens: 157045860 },
            rates: { prefillTokensPerSecond: 1553.2, generationTokensPerSecond: 32.5 },
            observedAt: Date.now()
        })
        await render(createElement(NodeInferenceMetricsSection, { nodeId: 'node-a' }))
        const rendered = text()
        expect(rendered).toContain("Engine's own counters (oMLX)")
        // The engine's counters include traffic PAIR did not route, so the
        // card has to say so: they are never mixed into PAIR's own numbers.
        expect(rendered).toContain('they include traffic PAIR did not route')
        expect(rendered).toContain('Requests (engine total) 3,270')
        expect(rendered).toContain('Prompt tokens (engine total) 212,978,711')
        expect(rendered).toContain('Cached tokens (engine total) 157,045,860')
        expect(rendered).toContain('Prefill (engine average) 1553 tok/s')
        expect(rendered).toContain('Generation (engine average) 33 tok/s')
    })

    it("shows the engine's view beside PAIR's own numbers, never mixed into them", async () => {
        nodeWithEngineNative({
            engine: 'omlx',
            supported: true,
            totals: { requests: 3270 },
            observedAt: Date.now()
        })
        useMetricsStore.setState({
            nodeInferenceMetrics: new Map([['node-a', nodeMetrics(reportedTotals)]])
        })
        await render(createElement(NodeInferenceMetricsSection, { nodeId: 'node-a' }))
        const rendered = text()
        expect(rendered).toContain('Prefill (last 60 s) 100 tok/s')
        expect(rendered).toContain("Engine's own counters (oMLX)")
        // PAIR's own token count is the sum of its own two counters; the
        // engine's request count is the engine's own and is never added to it.
        expect(rendered).toContain('Prefill + decode tokens (since broker start) 120')
        expect(rendered).toContain('Requests (engine total) 3,270')
    })

    it("shows how fresh the engine's own view is, and when it has gone stale", async () => {
        nodeWithEngineNative({
            engine: 'omlx',
            supported: true,
            totals: { requests: 2 },
            observedAt: Date.now()
        })
        await render(createElement(NodeInferenceMetricsSection, { nodeId: 'node-a' }))
        expect(text()).toContain('Observed just now.')

        // The engine stopped answering: the node keeps its last good view and
        // the stamp freezes, so the card has to say the view is stale.
        nodeWithEngineNative({
            engine: 'omlx',
            supported: true,
            totals: { requests: 2 },
            observedAt: Date.now() - 600_000
        })
        await render(createElement(NodeInferenceMetricsSection, { nodeId: 'node-a' }))
        expect(text()).toContain('Stale — last observed 10 minutes ago.')
    })

    it("keeps the engine's own model list apart from the loaded models", async () => {
        nodeWithEngineNative({
            engine: 'omlx',
            supported: true,
            models: [
                { model: 'mlx-community/Llama-3.2-3B-Instruct-4bit', loaded: true },
                { model: 'mlx-community/Qwen3-0.6B-4bit', loaded: false }
            ],
            observedAt: Date.now()
        })
        await render(createElement(NodeInferenceMetricsSection, { nodeId: 'node-a' }))
        const rendered = text()
        // The engine says which models it lists as active. The engine-manager's
        // loaded set says which are resident in memory, which is a different
        // thing and never says the same word here.
        expect(rendered).toContain('Models the engine lists:')
        expect(rendered).toContain('Llama 3.2 3B Instruct 4bit (active)')
        expect(rendered).toContain('Qwen3 0.6B 4bit (not active)')
    })

    it('renders nothing for a node whose engine has no native view', async () => {
        nodeWithEngineNative({ engine: 'ollama', supported: false })
        await render(createElement(NodeInferenceMetricsSection, { nodeId: 'node-a' }))
        expect(text()).toBe('')
    })

    it('renders nothing for a node that publishes no view at all', async () => {
        act(() => {
            useNodesStore.setState({ nodes: new Map() })
        })
        await render(createElement(NodeInferenceMetricsSection, { nodeId: 'node-a' }))
        expect(text()).toBe('')
    })

    it('keeps the engine section for a view whose counters are all zero', async () => {
        // The engine published a view and every counter in it is zero. The
        // counters are omitted from the wire (a reported zero and an
        // unreported counter are the same thing to a reader of a cumulative
        // counter), so the section has no rows — but the view exists, and the
        // freshness of the view is the signal that the engine is still
        // answering. The section must not disappear with the rows.
        nodeWithEngineNative({
            engine: 'omlx',
            supported: true,
            totals: {},
            rates: {},
            observedAt: Date.now()
        })
        await render(createElement(NodeInferenceMetricsSection, { nodeId: 'node-a' }))
        const rendered = text()
        expect(rendered).toContain("Engine's own counters (oMLX)")
        expect(rendered).toContain('Observed just now.')
    })
})
