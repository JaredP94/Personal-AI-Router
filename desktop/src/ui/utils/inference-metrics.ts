// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Formatting for the per-job and per-node inference metrics.
 *
 * The rule this file follows: Go owns all counting, all filtering, all
 * aggregation across records, and every windowed rate. The desktop never
 * counts, never filters and never aggregates across records — it renders
 * what the broker sent.
 *
 * Two figures are derived here, and both stay inside one record:
 *
 * - a phase's rate is the ratio of the tokens and the duration the engine
 *   reported for one model or one job, which is the same formula the broker
 *   uses for its own windowed rate, applied to one record;
 * - a token total adds the prefill and decode token counts of the same
 *   record.
 *
 * Every such ratio is labelled with the window it covers. The node's rates
 * are the broker's rolling-window rates, a model's ratios are its totals
 * since the broker started, and a job's ratios are that one request — so a
 * windowed rate and a since-broker-start ratio never appear under the same
 * label.
 *
 * Every formatter tells an absent measurement from a measured zero. An engine
 * that reports no phase split (LM Studio) leaves the measurement out
 * entirely, and that absence is a fact about the engine — not a zero. A
 * formatter that collapsed the two would report "0 tok/s" for an engine that
 * never reported a rate at all, which reads as a measurement the engine
 * never made.
 *
 * The optional extras are omitted rather than filled in. The job's
 * cached-token count and its total duration are values some engines never
 * report; a row for one of them is added only when the value is there, so an
 * engine that reports nothing extra shows no row for it.
 *
 * The node carries no cached-token row at all. The broker's node totals do
 * sum the cached counts, but they hold no counter for the requests that
 * reported one — so a node-level total would read as a measured zero on a
 * node whose engine never measured a cached count. The per-job row carries
 * the count where the engine did report it; a node-level total needs a
 * presence counter in the broker's totals, which the engine-cache work
 * brings.
 */
import type {
    InferenceMetricsTotals,
    NodeInferenceMetrics,
    NodeModelMetrics
} from '@/shared/types/metrics'
import type { Workload } from '@/shared/types/workloads'

/** The text used for a value the engine did not report. */
const NOT_REPORTED = 'not reported'

/**
 * The window the broker's node rates cover, as it appears in a label.
 *
 * The broker computes a node's rates over a 60-second rolling window (its
 * `nodeMetricsWindow` constant). The label says so, because the same
 * section also shows the totals since the broker started, and the two must
 * not read as the same measurement: a model that has gone idle keeps a high
 * ratio over those totals while the node's windowed rate falls to zero.
 */
const NODE_WINDOW_LABEL = 'last 60 s'

/**
 * The scope the broker's totals cover, as it appears in a label.
 *
 * The broker keeps its totals in memory and starts counting when it starts,
 * so they cover the time since the broker started — not the life of the
 * engine, and not the life of the machine. The label says so, because a user
 * who reads the total as covering the engine's whole life will draw the
 * wrong conclusion from a total that restarts with every broker.
 */
const TOTAL_LABEL = 'since broker start'

/** The window a single request's ratio covers, as it appears in a label. */
const REQUEST_LABEL = 'this request'

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
 * Whether the engine reported a prefill phase for this node.
 *
 * The presence counters answer it: a request that reported a prefill duration
 * increments `prefillRequests`, so a counter above zero means the engine
 * measured a prefill phase. The duration is consulted as well, so a snapshot
 * that carries a duration still counts even if its counters were lost.
 *
 * The duration alone cannot answer it: a reported prefill that took no time
 * sums to zero milliseconds, exactly like an engine that never reported a
 * prefill phase at all.
 */
export function hasPrefillMetrics(totals: InferenceMetricsTotals): boolean {
    return totals.prefillRequests > 0 || totals.prefillMs > 0
}

/**
 * Whether the engine reported a decode phase for this node. The same
 * counter-aware rule as {@link hasPrefillMetrics}.
 */
export function hasDecodeMetrics(totals: InferenceMetricsTotals): boolean {
    return totals.decodeRequests > 0 || totals.decodeMs > 0
}

/**
 * Whether the engine reported a time-to-first-token for this node.
 *
 * Only the counter answers it: `meanTtftMs` is a mean over the requests that
 * reported a TTFT, and it is zero when no request reported one — the same
 * number an engine reports when it measures a first token that took no time.
 * The counter is what separates the two.
 *
 * The counter is a lifetime figure — it counts every request that reported a
 * TTFT since the broker started — so it does not say whether the broker's
 * current window holds a measured TTFT. The node's TTFT row is answered by
 * the windowed mean for that reason.
 */
export function hasTtftMetrics(totals: InferenceMetricsTotals): boolean {
    return totals.ttftRequests > 0
}

/**
 * Whether the engine reported any phase split for this node.
 *
 * The presence counters answer it: a request that reported a prefill or a
 * decode duration increments the matching counter, so a counter above zero
 * means the engine measured a phase. The durations are consulted as well, so a
 * snapshot that carries a duration still counts even if its counters were
 * lost.
 *
 * The durations alone cannot answer it: a reported prefill that took no time
 * sums to zero milliseconds, exactly like an engine that never reported a
 * prefill phase at all.
 */
export function hasPhaseMetrics(totals: InferenceMetricsTotals): boolean {
    return hasPrefillMetrics(totals) || hasDecodeMetrics(totals)
}

/**
 * The rate of one phase, in tokens per second.
 *
 * This is the ratio of the tokens and the duration the engine reported for
 * one record — one model on one node, or one job. It is the same formula the
 * broker uses for its own windowed rate (`tokens / (duration / 1000)`),
 * applied to a single record, so nothing is summed across requests and no
 * rate is averaged. The caller labels the ratio with the window it covers.
 *
 * `null` means there is no rate to show: the engine reported no token count,
 * or it reported no duration for the phase. A reported duration of zero
 * yields a zero rate, the same answer the broker gives, because a phase that
 * took no time has no throughput — and never `Infinity` or `NaN`.
 */
export function phaseTokensPerSecond(
    tokens: number | null | undefined,
    durationMs: number | null | undefined
): number | null {
    if (tokens === null || tokens === undefined) return null
    if (durationMs === null || durationMs === undefined) return null
    if (durationMs <= 0) return 0
    return tokens / (durationMs / 1000)
}

/** One rendered metric: its label, the text to show, and whether it is absent. */
export interface InferenceMetricsRow {
    label: string
    value: string
    /** The engine never reported this value, so the text says so. */
    unavailable: boolean
}

/**
 * The tokens a node or a model handled: the prefill and decode token counts
 * the broker reported for that one record, added. The two counters describe
 * the same traffic, so the sum is that traffic's token volume — it is not a
 * total the broker keeps and the desktop reconstructs.
 *
 * The cached count is not part of the sum: cached tokens are a subset of
 * the prompt tokens, so adding them would count the same tokens twice.
 */
function reportedTokens(totals: InferenceMetricsTotals): number {
    return totals.prefillTokens + totals.decodeTokens
}

/**
 * The rows of a node's prefill/decode section.
 *
 * Every value here is the broker's own: the rates are the windowed rates it
 * computed, the token count is the total it has kept since it started. A row
 * is marked unavailable when the engine never reported that measurement at
 * all, which the presence counters answer — a phase the engine does not
 * measure must not read as a rate of zero.
 *
 * The TTFT row is always present, like the prefill and decode rows, and it
 * is answered by the broker's windowed mean rather than by the lifetime
 * counter: a node that measured a TTFT an hour ago and has been idle since
 * holds no measured TTFT in the broker's current window, and it must not
 * read as "0 ms" for a window in which nothing was measured. A measured
 * mean of exactly zero is not physically meaningful either — a
 * time-to-first-token is always positive — so a zero windowed mean means
 * "nothing was measured in this window".
 *
 * The rates are labelled with the broker's window, and the token count names
 * the two phases it adds and the scope the totals cover, so a windowed rate
 * and a since-broker-start count never read as the same figure.
 */
export function buildInferenceMetricsRows(metrics: NodeInferenceMetrics): InferenceMetricsRow[] {
    const { totals, rates } = metrics

    const prefill = hasPrefillMetrics(totals)
    const decode = hasDecodeMetrics(totals)
    const ttft = rates.meanTtftMs > 0
    const tokens = totals.tokenRequests > 0

    const rows: InferenceMetricsRow[] = [
        {
            label: `Prefill (${NODE_WINDOW_LABEL})`,
            value: prefill ? formatTokensPerSecond(rates.prefillTokensPerSecond) : NOT_REPORTED,
            unavailable: !prefill
        },
        {
            label: `Decode (${NODE_WINDOW_LABEL})`,
            value: decode ? formatTokensPerSecond(rates.decodeTokensPerSecond) : NOT_REPORTED,
            unavailable: !decode
        },
        {
            label: `TTFT (${NODE_WINDOW_LABEL})`,
            value: ttft ? formatDurationMs(rates.meanTtftMs) : NOT_REPORTED,
            unavailable: !ttft
        },
        {
            label: `Prefill + decode tokens (${TOTAL_LABEL})`,
            value: tokens ? formatTokenCount(reportedTokens(totals)) : NOT_REPORTED,
            unavailable: !tokens
        }
    ]
    return rows
}

/** One model's rows in the per-model breakdown of a node. */
export interface ModelMetricsRows {
    model: string
    rows: InferenceMetricsRow[]
}

/**
 * The per-model breakdown of a node.
 *
 * The broker keeps a total per model — one kept since the broker started —
 * but sends no rate for it: its rates are windowed over the whole node,
 * across every model it served. The rate of one model is therefore the ratio
 * of the tokens and the duration the broker reported for that model — one
 * record, no sum across requests and no average across models.
 *
 * The rows are labelled with the scope they cover, because the node's own
 * rows above them are labelled with the broker's window: a model that has
 * gone idle keeps a high ratio while the node's windowed rate falls to zero,
 * and the two must not read as the same measurement.
 */
export function buildModelMetricsRows(models: NodeModelMetrics[]): ModelMetricsRows[] {
    return models.map(entry => {
        const { totals } = entry

        const prefillRate = phaseTokensPerSecond(totals.prefillTokens, totals.prefillMs)
        const decodeRate = phaseTokensPerSecond(totals.decodeTokens, totals.decodeMs)
        const prefill = hasPrefillMetrics(totals) && prefillRate !== null
        const decode = hasDecodeMetrics(totals) && decodeRate !== null
        const tokens = totals.tokenRequests > 0

        return {
            model: entry.model,
            rows: [
                {
                    label: `Prefill (${TOTAL_LABEL})`,
                    value: prefill ? formatTokensPerSecond(prefillRate) : NOT_REPORTED,
                    unavailable: !prefill
                },
                {
                    label: `Decode (${TOTAL_LABEL})`,
                    value: decode ? formatTokensPerSecond(decodeRate) : NOT_REPORTED,
                    unavailable: !decode
                },
                {
                    label: `Prefill + decode tokens (${TOTAL_LABEL})`,
                    value: tokens ? formatTokenCount(reportedTokens(totals)) : NOT_REPORTED,
                    unavailable: !tokens
                }
            ]
        }
    })
}

/**
 * The measurement of one phase: the tokens the engine reported, the duration
 * it reported, and the rate that follows from the two.
 *
 * The rate is shown only when the engine reported a duration, because a rate
 * without a duration is not a rate. When the engine reported no duration at
 * all (an engine that reports token counts but no phase split), the timing
 * reads as "not reported" rather than as a zero — and the rate is left out
 * rather than shown as "0 tok/s".
 */
function phaseMeasurement(
    tokens: number | null | undefined,
    durationMs: number | null | undefined
): string {
    const tokenPart = isReported(tokens) ? `${formatTokenCount(tokens)} tok` : NOT_REPORTED
    const parts = [tokenPart, formatDurationMs(durationMs)]
    const rate = phaseTokensPerSecond(tokens, durationMs)
    if (rate !== null) {
        parts.push(formatTokensPerSecond(rate))
    }
    return parts.join(' · ')
}

/** Whether a value was reported at all, as opposed to being absent. */
function isReported(value: number | null | undefined): boolean {
    return value !== null && value !== undefined
}

/**
 * Whether the engine reported anything at all about this job.
 *
 * A job that is still running has no measurement: the numbers only exist
 * once the engine has answered. A job whose engine reported nothing reads
 * the same way, and neither shows a measurement row.
 */
export function hasWorkloadMeasurement(workload: Workload): boolean {
    return (
        isReported(workload.prefillTokens) ||
        isReported(workload.decodeTokens) ||
        isReported(workload.cachedTokens) ||
        isReported(workload.prefillMs) ||
        isReported(workload.decodeMs) ||
        isReported(workload.ttftMs) ||
        isReported(workload.totalMs)
    )
}

/**
 * The rows of a job's measurement: the prefill and decode tokens, the cached
 * count, the phase durations, the total and the rates that follow from them.
 *
 * Every number is the one the engine reported for this job. The rate is the
 * ratio of that job's tokens and duration, the same formula the broker uses
 * for its own rate, applied to one job — so the rows are labelled with the
 * request they cover.
 *
 * The phase rows are always there, because "this engine does not report
 * phases" is worth saying. The cached count and the total are optional
 * extras: their rows are added only when the engine reported the value, so
 * an absent value is left out rather than shown as a zero.
 */
export function buildWorkloadMetricsRows(workload: Workload): InferenceMetricsRow[] {
    const rows: InferenceMetricsRow[] = [
        {
            label: `Prefill (${REQUEST_LABEL})`,
            value: phaseMeasurement(workload.prefillTokens, workload.prefillMs),
            unavailable: !isReported(workload.prefillTokens) && !isReported(workload.prefillMs)
        },
        {
            label: `Decode (${REQUEST_LABEL})`,
            value: phaseMeasurement(workload.decodeTokens, workload.decodeMs),
            unavailable: !isReported(workload.decodeTokens) && !isReported(workload.decodeMs)
        },
        {
            label: `TTFT (${REQUEST_LABEL})`,
            value: formatDurationMs(workload.ttftMs),
            unavailable: !isReported(workload.ttftMs)
        }
    ]
    // The cached count is an optional extra: only oMLX reports it. The row
    // is omitted when the engine reported no count, so an absent value never
    // reads as a measured zero.
    if (isReported(workload.cachedTokens)) {
        rows.push({
            label: `Cached tokens (${REQUEST_LABEL})`,
            value: formatTokenCount(workload.cachedTokens),
            unavailable: false
        })
    }
    // The total is an optional extra too, and for the same reason: only an
    // engine that reports a total time fills it. The phase rows above stay
    // either way, because "this engine does not report phases" is worth
    // saying, while a missing total is simply not shown.
    if (isReported(workload.totalMs)) {
        rows.push({
            label: `Total (${REQUEST_LABEL})`,
            value: formatDurationMs(workload.totalMs),
            unavailable: false
        })
    }
    return rows
}
