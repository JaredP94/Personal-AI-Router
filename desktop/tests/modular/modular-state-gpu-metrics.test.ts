// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { NodeItemMetrics } from '@/shared/types/metrics'
import type { JsonObject } from '@/electron/service-bridge/json-rpc-subprocess'
import { subscribePush } from '@/electron/service-bridge/push-bus'
import { useMetricsStore } from '@/ui/stores/metrics.store'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'

const nodeId = 'gpu-semantics-test'
const updates: NodeItemMetrics[] = []
let unsubscribe = () => {}

function merge(gpus: JsonObject[]): void {
    getModularBridgeState().mergeNodeInfoResponse(nodeId, { GPUs: gpus })
}

function topology() {
    return getModularBridgeState().getNodesInitial().nodes[nodeId].topology.gpus
}

function current() {
    return updates[updates.length - 1].current
}

describe('GPU memory semantics from node-info through metrics history', () => {
    beforeEach(() => {
        vi.useFakeTimers()
        vi.setSystemTime(100_000)
        getModularBridgeState().handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [{ hostUuid: nodeId, name: 'fixture', ipAddress: '192.0.2.1', port: 14318 }]
            }
        })
        updates.length = 0
        unsubscribe = subscribePush(event => {
            if (event.channel === 'metrics:update') updates.push(event.payload)
        })
        vi.stubGlobal('window', {
            pairApi: {
                metrics: {
                    onRoutingUpdate: () => () => {},
                    getRouting: async () => [],
                    onUpdate: (callback: (metrics: NodeItemMetrics) => void) =>
                        subscribePush(event => {
                            if (event.channel === 'metrics:update') callback(event.payload)
                        })
                },
                nodes: { onRemove: () => () => {} }
            }
        })
        useMetricsStore.getState().clearAll()
        useMetricsStore.getState().initialize()
    })

    afterEach(() => {
        unsubscribe()
        useMetricsStore.getState().cleanup()
        vi.unstubAllGlobals()
        vi.useRealTimers()
    })

    it.each(['dedicated', 'unified', 'shared', 'unavailable'])(
        'preserves the service-provided %s kind without name inference',
        memoryKind => {
            merge([
                {
                    name: 'Apple NVIDIA fixture',
                    memory_kind: memoryKind,
                    vram_bytes: 800,
                    shared_bytes: 400
                }
            ])
            expect(topology()[0]).toEqual({
                id: `${nodeId}:gpu:0`,
                name: 'Apple NVIDIA fixture',
                memoryKind,
                vramTotal: 800,
                sharedTotal: 400
            })
        }
    )

    it('treats legacy and unrecognized kinds as unavailable and missing readings as null', () => {
        merge([{ name: 'Legacy' }, { name: 'Future', memory_kind: 'future' }])
        expect(topology().map(gpu => gpu.memoryKind)).toEqual(['unavailable', 'unavailable'])
        expect(topology().map(gpu => [gpu.vramTotal, gpu.sharedTotal])).toEqual([
            [null, null],
            [null, null]
        ])
        expect(current().gpuUtilization.map(gpu => gpu.value)).toEqual([null, null])
        expect(current().gpuVramUsage.map(gpu => gpu.value)).toEqual([null, null])
    })

    it('keeps idle zero distinct from a missing adapter reading', () => {
        merge([
            {
                name: 'Idle',
                memory_kind: 'dedicated',
                vram_bytes: 800,
                vram_used_bytes: 0,
                utilization_percent: 0
            },
            { name: 'Unknown', memory_kind: 'unavailable' }
        ])
        expect(current().gpuUtilization.map(gpu => gpu.value)).toEqual([0, null])
        expect(current().gpuVramUsage.map(gpu => gpu.value)).toEqual([0, null])
    })

    it('requires both measured usage and positive capacity to compute a percentage', () => {
        merge([
            { name: 'Known', vram_bytes: 800, vram_used_bytes: 200 },
            { name: 'Capacity only', vram_bytes: 800 },
            { name: 'Used only', vram_used_bytes: 200 },
            { name: 'Zero capacity', vram_bytes: 0, vram_used_bytes: 0 },
            {
                name: 'Malformed',
                vram_bytes: '800',
                vram_used_bytes: '200',
                utilization_percent: '0'
            }
        ])
        expect(current().gpuVramUsage.map(gpu => gpu.value)).toEqual([25, null, null, null, null])
        expect(current().gpuUtilization.map(gpu => gpu.value)).toEqual([
            null,
            null,
            null,
            null,
            null
        ])
    })

    it('propagates kind-only and shared-only changes instead of retaining an equal-looking snapshot', () => {
        merge([{ name: 'GPU', memory_kind: 'dedicated', shared_bytes: 400, shared_used_bytes: 0 }])
        expect(current().gpuVramUsage[0].sharedUsed).toBe(0)
        vi.advanceTimersByTime(1000)
        merge([{ name: 'GPU', memory_kind: 'shared', shared_bytes: 400, shared_used_bytes: 0 }])
        expect(topology()[0].memoryKind).toBe('shared')
        vi.advanceTimersByTime(1000)
        merge([{ name: 'GPU', memory_kind: 'shared', shared_bytes: 800, shared_used_bytes: 0 }])
        expect(topology()[0].sharedTotal).toBe(800)
        vi.advanceTimersByTime(1000)
        merge([{ name: 'GPU', memory_kind: 'shared', shared_bytes: 800, shared_used_bytes: 100 }])
        expect(current().gpuVramUsage[0].sharedUsed).toBe(100)
        vi.advanceTimersByTime(1000)
        merge([{ name: 'GPU', memory_kind: 'shared', shared_bytes: 800 }])
        expect(current().gpuVramUsage[0].sharedUsed).toBeNull()
    })

    it('starts history with only real samples, then appends null gaps and recovers', () => {
        merge([
            {
                name: 'GPU',
                memory_kind: 'dedicated',
                vram_bytes: 800,
                vram_used_bytes: 200,
                utilization_percent: 25,
                shared_used_bytes: 100
            }
        ])
        const first = useMetricsStore.getState().nodeMetrics.get(nodeId)
        expect(first?.gpuUtilization[0].data).toEqual([{ timestamp: 100_000, value: 25 }])
        expect(first?.cpuUtilization).toHaveLength(1)
        expect(first?.memoryUsage).toHaveLength(1)
        vi.advanceTimersByTime(1000)
        merge([{ name: 'GPU', memory_kind: 'dedicated', vram_bytes: 800 }])
        vi.advanceTimersByTime(1000)
        merge([
            {
                name: 'GPU',
                memory_kind: 'dedicated',
                vram_bytes: 800,
                vram_used_bytes: 0,
                utilization_percent: 0,
                shared_used_bytes: 0
            }
        ])
        const history = useMetricsStore.getState().nodeMetrics.get(nodeId)
        expect(history?.gpuUtilization[0].data.map(point => point.value)).toEqual([25, null, 0])
        expect(history?.gpuVramUsage[0].data.map(point => point.value)).toEqual([25, null, 0])
        expect(history?.gpuVramUsage[0].sharedUsed.map(point => point.value)).toEqual([
            100,
            null,
            0
        ])
    })
})
