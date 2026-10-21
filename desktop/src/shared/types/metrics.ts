// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export interface GpuMetricValue {
    id: string // GPU ID from topology
    value: number | null // percentage, null when not measured
}

interface GpuMemoryMetricValue extends GpuMetricValue {
    sharedUsed: number | null // Windows shared-system-memory usage, in bytes
}

export interface NodeItemMetricsEntry {
    timestamp: number // milliseconds since epoch
    cpuUtilization: number // percentage
    memoryUsage: number // percentage
    gpuUtilization: GpuMetricValue[] // percentage per GPU
    gpuVramUsage: GpuMemoryMetricValue[] // percentage per GPU, plus shared usage in bytes
}

export interface NodeItemMetrics {
    id: string
    current: NodeItemMetricsEntry
    historical: NodeItemMetricsEntry[]
}

/**
 * Totals of the prefill/decode measurements the broker observed for one
 * node, or for one model on that node, accumulated since the broker started.
 *
 * Every value is the broker's own — the desktop renders them and never
 * derives one. The presence counters say how many requests actually reported
 * each value: a counter of zero means the engine never reported that value
 * at all, which is different from a reported zero (a completion with no
 * output tokens, say). A consumer that ignores the counters cannot tell
 * "this engine reports no phase split" from "this engine is idle", and would
 * render the first as a real `0 tok/s`.
 */
export interface InferenceMetricsTotals {
    requests: number
    prefillTokens: number
    decodeTokens: number
    cachedTokens: number
    prefillMs: number
    decodeMs: number
    ttftMs: number
    /** Requests that reported a time-to-first-token. */
    ttftRequests: number
    /** Requests that reported at least one token count. */
    tokenRequests: number
    /** Requests that reported a prefill duration. */
    prefillRequests: number
    /** Requests that reported a decode duration. */
    decodeRequests: number
}

/**
 * A node's current throughput, computed by the broker over its rolling
 * window. A rate whose denominator is zero is zero, never NaN.
 */
export interface InferenceMetricsRates {
    prefillTokensPerSecond: number
    decodeTokensPerSecond: number
    meanTtftMs: number
}

/** One model's totals since the broker started on one node. */
export interface NodeModelMetrics {
    model: string
    totals: InferenceMetricsTotals
}

/** One node's inference metrics: totals, current rates and the per-model rows. */
export interface NodeInferenceMetrics {
    nodeId: string
    totals: InferenceMetricsTotals
    rates: InferenceMetricsRates
    models: NodeModelMetrics[]
}

/**
 * The full per-node snapshot: the response of `metrics:get-node-metrics` and
 * the payload of the `metrics:node-metrics` push. It is always the whole
 * snapshot, never a delta.
 */
export interface NodeInferenceMetricsSnapshot {
    nodes: NodeInferenceMetrics[]
}
