// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { create } from 'zustand'
import type { RoutingMetrics } from '@/shared/types/routing-metrics'
import type {
    NodeInferenceMetrics,
    NodeInferenceMetricsSnapshot,
    NodeItemMetrics
} from '@/shared/types/metrics'
import type { PerformanceMetric } from '@/ui/types/types'

const MAX_DATA_POINTS = 60

/**
 * How often the store re-reads the per-node inference metrics snapshot.
 *
 * The broker pushes the snapshot only when a measurement was recorded, and
 * the rates it computes are windowed: a node's rate decays towards zero as
 * its 60-second window empties, and that decay is never pushed because
 * nothing was measured. A push-only client therefore shows the last pushed
 * rate until the next completion — a stale number presented as a current
 * one.
 *
 * The store therefore re-reads the snapshot on an interval instead of
 * relying on the pushes alone. The read goes to the broker (the invoke
 * handler asks the broker directly), so the re-read returns the broker's
 * current rates rather than the last pushed ones. Five seconds keeps the
 * displayed rate within five seconds of the broker's own computation, which
 * is well inside the window it is computed over, and keeps the read a light
 * RPC.
 */
const NODE_METRICS_REFRESH_MS = 5_000

interface GpuMetricsHistory {
    id: string
    data: PerformanceMetric[]
}

interface GpuMemoryMetricsHistory extends GpuMetricsHistory {
    sharedUsed: PerformanceMetric[] // bytes, not percentage
}

export interface NodeMetricsHistory {
    gpuUtilization: GpuMetricsHistory[]
    gpuVramUsage: GpuMemoryMetricsHistory[]
    cpuUtilization: PerformanceMetric[]
    memoryUsage: PerformanceMetric[]
}

interface MetricsStore {
    routingMetrics: RoutingMetrics[]
    nodeMetrics: Map<string, NodeMetricsHistory>
    /**
     * The latest per-node inference metrics, keyed by node id. The values are
     * the broker's own: the desktop never sums, averages or derives a rate
     * from them.
     */
    nodeInferenceMetrics: Map<string, NodeInferenceMetrics>
    generation: number
    initialize: () => void
    clearAll: () => void
    cleanup: () => void
}

let unsubs: Array<() => void> = []
let nodeMetricsTimer: ReturnType<typeof setInterval> | null = null

function pushMetric(arr: PerformanceMetric[], value: number | null, timestamp: number): void {
    if (arr.length >= MAX_DATA_POINTS) {
        arr.shift()
    }
    arr.push({ timestamp, value })
}

/** Key a per-node inference metrics snapshot by node id. */
function toNodeInferenceMetrics(
    snapshot: NodeInferenceMetricsSnapshot
): Map<string, NodeInferenceMetrics> {
    const map = new Map<string, NodeInferenceMetrics>()
    for (const node of snapshot.nodes) {
        map.set(node.nodeId, node)
    }
    return map
}

/**
 * Store a per-node inference metrics snapshot. A push replaces the whole map
 * — the broker's snapshot is a state synchronization, not a delta.
 */
function applyNodeMetrics(snapshot: NodeInferenceMetricsSnapshot): void {
    useMetricsStore.setState(state => ({
        nodeInferenceMetrics: toNodeInferenceMetrics(snapshot),
        generation: state.generation + 1
    }))
}

/**
 * Re-read the per-node inference metrics snapshot from the broker. A failed
 * read keeps the last snapshot: an empty one would read as "this node never
 * served anything".
 */
function readNodeMetrics(): void {
    if (typeof window === 'undefined' || !window.pairApi) return
    void window.pairApi.metrics
        .getNodeMetrics()
        .then(applyNodeMetrics)
        .catch(() => {})
}

export const useMetricsStore = create<MetricsStore>((set, get) => ({
    routingMetrics: [],
    nodeMetrics: new Map(),
    nodeInferenceMetrics: new Map(),
    generation: 0,

    clearAll: () => {
        set({
            nodeMetrics: new Map(),
            nodeInferenceMetrics: new Map(),
            generation: get().generation + 1
        })
        // The snapshot is the broker's current state, not a history: drop it
        // and read it back rather than leaving the view empty until the next
        // interval tick.
        readNodeMetrics()
    },

    initialize: () => {
        if (!window.pairApi) return

        let receivedRoutingUpdate = false
        unsubs.push(
            window.pairApi.metrics.onRoutingUpdate(routingMetrics => {
                receivedRoutingUpdate = true
                set({ routingMetrics })
            })
        )
        void window.pairApi.metrics
            .getRouting()
            .then(routingMetrics => {
                if (!receivedRoutingUpdate) set({ routingMetrics })
            })
            .catch(() => {})

        // Per-node inference metrics. The push keeps the view current when a
        // measurement lands; the interval below keeps the rates honest,
        // because the broker's push is measurement-driven and the rates it
        // computes decay between measurements.
        unsubs.push(
            window.pairApi.metrics.onNodeMetrics(snapshot => {
                applyNodeMetrics(snapshot)
            })
        )
        readNodeMetrics()
        if (nodeMetricsTimer) clearInterval(nodeMetricsTimer)
        nodeMetricsTimer = setInterval(() => readNodeMetrics(), NODE_METRICS_REFRESH_MS)

        unsubs.push(
            window.pairApi.nodes.onRemove((nodeId: string) => {
                const map = get().nodeMetrics
                if (map.has(nodeId)) {
                    map.delete(nodeId)
                    set({ generation: get().generation + 1 })
                }
            }),
            window.pairApi.metrics.onUpdate((metrics: NodeItemMetrics) => {
                const map = get().nodeMetrics
                let history = map.get(metrics.id)

                if (!history) {
                    history = {
                        gpuUtilization: [],
                        gpuVramUsage: [],
                        cpuUtilization: [],
                        memoryUsage: []
                    }
                    map.set(metrics.id, history)
                }

                const lastTs = history.cpuUtilization[history.cpuUtilization.length - 1]?.timestamp
                if (lastTs === metrics.current.timestamp) return

                const ts = metrics.current.timestamp

                for (const gpu of metrics.current.gpuUtilization) {
                    let entry = history.gpuUtilization.find(h => h.id === gpu.id)
                    if (!entry) {
                        entry = { id: gpu.id, data: [] }
                        history.gpuUtilization.push(entry)
                    }
                    pushMetric(entry.data, gpu.value, ts)
                }

                for (const gpu of metrics.current.gpuVramUsage) {
                    let entry = history.gpuVramUsage.find(h => h.id === gpu.id)
                    if (!entry) {
                        entry = { id: gpu.id, data: [], sharedUsed: [] }
                        history.gpuVramUsage.push(entry)
                    }
                    pushMetric(entry.data, gpu.value, ts)
                    pushMetric(entry.sharedUsed, gpu.sharedUsed, ts)
                }

                pushMetric(history.cpuUtilization, metrics.current.cpuUtilization, ts)
                pushMetric(history.memoryUsage, metrics.current.memoryUsage, ts)

                map.set(metrics.id, { ...history })
                set({ generation: get().generation + 1 })
            })
        )
    },

    cleanup: () => {
        unsubs.forEach(u => u())
        unsubs = []
        if (nodeMetricsTimer) {
            clearInterval(nodeMetricsTimer)
            nodeMetricsTimer = null
        }
    }
}))
