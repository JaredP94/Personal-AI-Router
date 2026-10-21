// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { Workload, WorkloadLiveProgress } from '@/shared/types/workloads'
import type { EngineType } from '@/shared/types/engines'

/**
 * Stable catalog key for a workload.
 *
 * The backend's catalog is keyed by `(originatedFrom, engine, runId, id)`, but
 * the `workloads:remove` push carries only `(workloadId, originatedFrom)` — and
 * the broker's own `Store.Remove` drops every record matching that pair — so
 * `(originatedFrom, id)` is the only key a subscribe client can maintain
 * consistently across upsert and remove. Each node's proxy assigns workload ids
 * from its own monotonic counter, so ids collide across nodes; `originatedFrom`
 * (the origin node) disambiguates. Mirror that here so a remote node's job never
 * overwrites a local one that happens to share an id. The `\u0000` separator
 * cannot appear in a host id or proxy counter, so the key is unambiguous.
 */
export function workloadKey(originatedFrom: string | null, id: string): string {
    return `${originatedFrom ?? ''}\u0000${id}`
}

/**
 * Stable key for the live view of a running job.
 *
 * The live view is keyed by the origin, the engine and the job id — the same
 * identity the live frame carries. The engine is part of it because every
 * proxy counts its own requests from 1: an oMLX job "1" and an Ollama job "1"
 * on one node are two different jobs, and a live view keyed without the engine
 * would show one job's counts on the other job's card.
 *
 * The catalog key is NOT used here: it leaves the engine out (the removal
 * push names no engine), so it cannot tell two engines' jobs apart.
 */
export function liveProgressKey(
    originatedFrom: string | null,
    engine: EngineType | null,
    id: string
): string {
    return `${originatedFrom ?? ''}\u0000${engine ?? ''}\u0000${id}`
}

/**
 * Whether a live key belongs to the job `(originatedFrom, id)` on any engine.
 *
 * A removal names no engine — the removal wire carries only the job id and
 * the origin — so it retires the live view of the job on every engine of
 * that origin.
 */
export function liveProgressKeyIsJob(
    key: string,
    originatedFrom: string | null,
    id: string
): boolean {
    return key.startsWith(`${originatedFrom ?? ''}\u0000`) && key.endsWith(`\u0000${id}`)
}

/**
 * Which node a workload actually ran on, for UI attribution.
 *
 * `scheduledOn` is the node the scheduler routed the request to (where it
 * actually runs); `originatedFrom` is only where the request entered the
 * cluster. UI that means "which node is this job on" (per-node job badges, the
 * workload→node connection lines, the "ran on" label) attributes strictly to
 * `scheduledOn`. A `null` result means the workload has **not been scheduled
 * yet** (still queued) — it has no run-node, so it draws no connection line, is
 * counted on no node, and shows no "ran on" target. Deliberately does NOT fall
 * back to `originatedFrom`: the origin is where the request came from, not where
 * the job ran.
 */
export function workloadExecutionNodeId(workload: Pick<Workload, 'scheduledOn'>): string | null {
    return workload.scheduledOn ?? null
}

/**
 * Merge a live progress frame into the view a job already has.
 *
 * A frame is a PARTIAL view of a running request: the origin proxy publishes
 * what PAIR measured of the client's stream and the serving node publishes
 * what the engine reported of its own request, and neither sees the other's
 * facts. So a field the incoming frame does not carry leaves the stored value
 * alone — "not measured here" is never a zero — and a field it does carry
 * overwrites it.
 *
 * The engine's reading is the one exception: it is a single reading of a
 * single engine row, so it is replaced as a unit rather than merged field by
 * field. A later row supersedes the earlier one — it does not blend with it,
 * and a field the engine has stopped reporting stops being shown.
 */
export function mergeWorkloadLiveProgress(
    stored: WorkloadLiveProgress | undefined,
    incoming: WorkloadLiveProgress
): WorkloadLiveProgress {
    const merged: WorkloadLiveProgress = {
        jobId: incoming.jobId,
        originatedFrom: incoming.originatedFrom ?? stored?.originatedFrom ?? null,
        engine: incoming.engine ?? stored?.engine ?? null,
        nodeId: incoming.nodeId ?? stored?.nodeId ?? null,
        elapsedMs: incoming.elapsedMs ?? stored?.elapsedMs ?? null,
        chunks: incoming.chunks ?? stored?.chunks ?? null,
        bytes: incoming.bytes ?? stored?.bytes ?? null,
        ttfbMs: incoming.ttfbMs ?? stored?.ttfbMs ?? null,
        engineProgress: incoming.engineProgress ?? stored?.engineProgress ?? null
    }
    return merged
}
