// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { Workload, WorkloadLiveProgress } from '@/shared/types/workloads'

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
 * The engine's object is the one exception: it is a single reading of a
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
        nodeId: incoming.nodeId ?? stored?.nodeId ?? null,
        elapsedMs: incoming.elapsedMs ?? stored?.elapsedMs ?? null,
        chunks: incoming.chunks ?? stored?.chunks ?? null,
        bytes: incoming.bytes ?? stored?.bytes ?? null,
        ttfbMs: incoming.ttfbMs ?? stored?.ttfbMs ?? null,
        engine: incoming.engine ?? stored?.engine ?? null
    }
    return merged
}
