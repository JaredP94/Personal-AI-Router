// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { Workload, WorkloadLiveProgress, WorkloadRemoval } from '@/shared/types/workloads'
import type { EngineType } from '@/shared/types/engines'

/** The fields that identify one job. */
interface WorkloadIdentity {
    originatedFrom: string | null
    engine: EngineType | null
    runId?: string | null
    id: string
}

/**
 * Stable catalog key for a workload: the backend's own identity,
 * `(originatedFrom, engine, runId, id)`.
 *
 * Every proxy numbers its jobs from 1, and numbers again from 1 when it
 * restarts. The origin tells nodes apart, the engine tells an Ollama job "3"
 * from an oMLX job "3" on one node, and the run id tells a job from the one a
 * restarted proxy gave the same number. Keyed on fewer fields, one job's card
 * would overwrite another's. The `\u0000` separator cannot appear in any of
 * the fields, so the key is unambiguous.
 */
export function workloadKey(workload: WorkloadIdentity): string {
    return [
        workload.originatedFrom ?? '',
        workload.engine ?? '',
        workload.runId ?? '',
        workload.id
    ].join('\u0000')
}

/**
 * Whether a removal names this workload. A removal from the broker carries
 * only `(workloadId, originatedFrom)` and, like the broker's own store, drops
 * every job matching that pair. One raised by the desktop itself names the
 * engine and run too, so it drops exactly one job.
 */
export function workloadMatchesRemoval(
    workload: WorkloadIdentity,
    removal: WorkloadRemoval
): boolean {
    if (workload.id !== removal.workloadId) return false
    if ((workload.originatedFrom ?? '') !== (removal.originatedFrom ?? '')) return false
    if (removal.engine != null && workload.engine !== removal.engine) return false
    if (removal.runId != null && (workload.runId ?? null) !== removal.runId) return false
    return true
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
 *
 * The run id is the other exception. A proxy that restarts counts its jobs
 * from 1 again, so the same job id can name a DIFFERENT job: when the
 * incoming frame names another run, the stored view belongs to the job it
 * replaced and is replaced rather than merged. The broker merges the same
 * way, so the desktop and the broker agree on which job a frame is about.
 */
export function mergeWorkloadLiveProgress(
    stored: WorkloadLiveProgress | undefined,
    incoming: WorkloadLiveProgress
): WorkloadLiveProgress {
    const sameRun = !incoming.runId || !stored?.runId || incoming.runId === stored.runId
    const base = sameRun ? stored : undefined
    const merged: WorkloadLiveProgress = {
        jobId: incoming.jobId,
        originatedFrom: incoming.originatedFrom ?? base?.originatedFrom ?? null,
        engine: incoming.engine ?? base?.engine ?? null,
        runId: incoming.runId ?? base?.runId ?? null,
        nodeId: incoming.nodeId ?? base?.nodeId ?? null,
        elapsedMs: incoming.elapsedMs ?? base?.elapsedMs ?? null,
        chunks: incoming.chunks ?? base?.chunks ?? null,
        bytes: incoming.bytes ?? base?.bytes ?? null,
        ttfbMs: incoming.ttfbMs ?? base?.ttfbMs ?? null,
        engineProgress: incoming.engineProgress ?? base?.engineProgress ?? null
    }
    return merged
}
