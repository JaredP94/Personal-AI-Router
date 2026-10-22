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

/**
 * The engine's own counters, as the engine published them.
 *
 * Every field is optional: the engine reports a counter with `omitempty`, so
 * an absent key means the engine did not report that value — never a
 * measured zero. The object itself is always present on the wire, but it can
 * be empty, so an empty object means "the engine reported no numbers", not
 * "the engine reported zeros".
 */
export interface EngineNativeTotals {
    /** Requests the engine has served since it started. */
    requests?: number
    /** Prompt tokens the engine has processed. */
    promptTokens?: number
    /** Completion tokens the engine has produced. */
    completionTokens?: number
    /** Prompt tokens the engine served from its prefix cache. */
    cachedTokens?: number
    /** The share of prompt tokens served from the cache, in percent. */
    cacheEfficiency?: number
}

/**
 * The engine's own throughput numbers: the averages it computes over its own
 * counters.
 *
 * Every field is optional, for the same reason as in
 * {@link EngineNativeTotals}: an absent key means the engine did not report
 * that value.
 */
export interface EngineNativeRates {
    /** The engine's average prefill throughput, in tokens per second. */
    prefillTokensPerSecond?: number
    /** The engine's average generation throughput, in tokens per second. */
    generationTokensPerSecond?: number
}

/**
 * One model the engine lists as active.
 *
 * `loaded` is the engine's own claim about the model: it means the engine
 * lists this model as active, not that the model is resident in memory. The
 * engine-manager's per-engine loaded set is a different thing — the models
 * resident in memory — and the two must not be shown under the same word.
 */
export interface EngineNativeModel {
    model: string
    loaded: boolean
}

/**
 * The engine's own view of itself: the counters, the rates and the models it
 * lists as active, as the engine published them.
 *
 * These are the engine's numbers, not PAIR's. They include the traffic PAIR
 * did not route, so a consumer presents them beside PAIR's own counters and
 * never merges, sums or averages the two.
 *
 * `supported` says whether this engine has a native view at all. An engine
 * without an admin API publishes none, and a consumer renders nothing for it
 * — no section and no "not reported" noise.
 */
export interface EngineNativeView {
    /** The engine that published the view, e.g. "omlx". */
    engine: string
    /** Whether this engine has a native view of itself. */
    supported: boolean
    totals?: EngineNativeTotals
    rates?: EngineNativeRates
    models?: EngineNativeModel[]
    /**
     * When the view was observed, in milliseconds since the epoch.
     *
     * The stamp advances on every sweep, so it is the freshness signal: a
     * node whose engine stopped answering keeps its last good view with a
     * frozen stamp, and the age of that stamp is how a dead engine shows.
     */
    observedAt?: number
}
