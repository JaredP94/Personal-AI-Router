// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { WorkloadStates } from '@/shared/constants/workloads'
import { EngineType } from '@/shared/types/engines'

export type WorkloadState = (typeof WorkloadStates)[number]

export interface Workload {
    id: string
    model: string
    engine: EngineType
    state: WorkloadState
    /**
     * Owner/origin node of the workload — the node whose proxy received the
     * request. This is the identity half of the backend's `(originatedFrom, id)`
     * global catalog key (workload ids are a per-node proxy counter, so they
     * collide across nodes; `originatedFrom` disambiguates).
     */
    originatedFrom: string | null
    /**
     * Node the workload was routed to / scheduled on — where it actually ran,
     * as opposed to `originatedFrom` (where it came from). Optional/additive:
     * absent until the scheduler picks a target (e.g. a still-queued workload).
     * Use `workloadExecutionNodeId` for "which node is this job on" UI.
     */
    scheduledOn?: string | null
    createdAt: number
    startedAt: number | null
    completedAt: number | null
    error: string | null
    requesterId: string | null
    /**
     * The engine's own prefill/decode measurement for this job, as the
     * engine reported it. Every field is optional and `null` when the engine
     * reported nothing: the measurement rides the workload object inline, and
     * an engine that reports no phase split (LM Studio) leaves the field
     * absent. `null` means "not reported by this engine", which is different
     * from a measured `0`.
     */
    prefillTokens?: number | null
    decodeTokens?: number | null
    cachedTokens?: number | null
    prefillMs?: number | null
    decodeMs?: number | null
    ttftMs?: number | null
    totalMs?: number | null
}

/**
 * The engine's own live reading of one in-flight request.
 *
 * Every number in it was measured by the engine, and only what the engine
 * reported is present: a reading it did not report stays absent (`null`)
 * rather than becoming a zero. A chunk is not a token, so this object never
 * holds a token count the engine did not report.
 */
export interface WorkloadLiveEngine {
    /** The engine's own request id — the id the engine's live rows are keyed by. */
    requestId: string
    /** The phase the engine reported the request in: waiting, prefilling, generating. */
    phase: string
    /** The number of tokens the engine has generated. */
    generatedTokens?: number | null
    /** The engine's own generation rate. */
    tokensPerSecond?: number | null
    /** The engine's own prefill progress. */
    prefillProgress?: number | null
    /** The engine's own queue position. */
    queuePosition?: number | null
}

/**
 * The live view of one running inference request.
 *
 * It is a PARTIAL view: the origin proxy publishes what PAIR measured of the
 * client's stream (the elapsed time, the content chunks received, the
 * response bytes received, the time to the first byte) and the serving node
 * publishes what the engine reported of its own request. Neither publisher
 * sees the other's facts, so a field a frame does not carry means "not
 * measured here" — never zero.
 *
 * `jobId` plus `originatedFrom` plus `engine` is the job's identity, the same
 * triple the live view is keyed by: PAIR's job id is a per-proxy counter, so
 * two origins can both hold a job "1", and two engines on one node each
 * count from 1 as well.
 */
export interface WorkloadLiveProgress {
    /** PAIR's own id for the request — the same id the workload carries. */
    jobId: string
    /**
     * The node the request entered the cluster on — the job's origin.
     *
     * The broker stamps it on every frame (the same way it stamps
     * `originatedFrom` onto a workload lifecycle event), so it is always
     * present; a frame that somehow carries none joins no job.
     */
    originatedFrom: string | null
    /**
     * The engine that serves the job — the same engine the workload
     * carries.
     *
     * It is part of the job's identity: every proxy counts its own requests
     * from 1, so an oMLX job "1" and an Ollama job "1" on one node are two
     * different jobs, and only the engine tells them apart.
     */
    engine: EngineType | null
    /**
     * The proxy's per-process run nonce — the same value the workload
     * carries as `runId`.
     *
     * It tells a recycled job id from the job it replaced: a proxy that
     * restarts counts its jobs from 1 again. A frame that names another
     * run is a different job, and the stored view of the old one is
     * replaced rather than merged into it.
     */
    runId?: string | null
    /** The node that is serving the request. */
    nodeId?: string | null
    /** The time since the request reached the proxy. */
    elapsedMs?: number | null
    /** The number of content chunks received so far. */
    chunks?: number | null
    /** The number of response body bytes received so far. */
    bytes?: number | null
    /** The time to the first response byte. */
    ttfbMs?: number | null
    /**
     * The engine's own live reading, absent when the engine reported
     * nothing.
     */
    engineProgress?: WorkloadLiveEngine | null
}
