// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Formatting for the per-job and per-node inference metrics.
 *
 * The desktop renders what the broker reported; it never sums, averages or
 * derives a rate. These helpers only turn a reported number into text.
 *
 * Every formatter tells an absent measurement from a measured zero. An engine
 * that reports no phase split (LM Studio) leaves the measurement out
 * entirely, and that absence is a fact about the engine — not a zero. A
 * formatter that collapsed the two would report "0 tok/s" for an engine that
 * never reported a rate at all, which reads as a measurement the engine
 * never made.
 */
import type { InferenceMetricsTotals } from '@/shared/types/metrics'

/** The text used for a value the engine did not report. */
const NOT_REPORTED = 'not reported'

/**
 * Format a token count with thousands separators.
 *
 * An absent count (`null` / `undefined`) reads as "not reported"; a reported
 * zero reads as "0".
 */
export function formatTokenCount(value: number | null | undefined): string {
    if (value === null || value === undefined) return NOT_REPORTED
    return value.toLocaleString('en-US')
}

/**
 * Format a throughput figure in tokens per second, rounded to the nearest
 * integer.
 *
 * An absent rate reads as "not reported"; a reported zero reads as "0 tok/s".
 */
export function formatTokensPerSecond(value: number | null | undefined): string {
    if (value === null || value === undefined) return NOT_REPORTED
    return `${Math.round(value)} tok/s`
}

/**
 * Format a duration in milliseconds: under a second as whole milliseconds
 * ("999 ms"), a second and above as seconds with one decimal ("1.5 s").
 *
 * An absent duration reads as "not reported"; a reported zero reads as
 * "0 ms".
 */
export function formatDurationMs(value: number | null | undefined): string {
    if (value === null || value === undefined) return NOT_REPORTED
    if (Math.abs(value) < 1000) return `${Math.round(value)} ms`
    const seconds = (value / 1000).toFixed(1).replace(/\.0$/, '')
    return `${seconds} s`
}

/**
 * Whether the engine reported a prefill or a decode phase for this node.
 *
 * The presence counters answer it: a request that reported a prefill duration
 * increments `prefillRequests`, so a counter above zero means the engine
 * measured a prefill phase. The durations are consulted as well, so a
 * snapshot that carries a duration still counts even if its counters were
 * lost.
 *
 * The durations alone cannot answer it: a reported prefill that took no time
 * sums to zero milliseconds, exactly like an engine that never reported a
 * prefill phase at all.
 */
export function hasPhaseMetrics(totals: InferenceMetricsTotals): boolean {
    return (
        totals.prefillRequests > 0 ||
        totals.decodeRequests > 0 ||
        totals.prefillMs > 0 ||
        totals.decodeMs > 0
    )
}
