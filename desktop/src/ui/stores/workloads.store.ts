// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { create } from 'zustand'
import { useShallow } from 'zustand/react/shallow'
import type { Workload, WorkloadLiveProgress } from '@/shared/types/workloads'
import deepEqual from '@/ui/utils/deep-equal'
import {
    mergeWorkloadLiveProgress,
    workloadExecutionNodeId,
    workloadKey
} from '@/shared/utils/workloads'
import { stateOrder, MAX_HISTORY_ITEMS } from '@/ui/constants/app'

interface WorkloadsStore {
    workloads: Map<string, Workload>
    /**
     * The live view of every running job, keyed like the catalog
     * (`originatedFrom` + `id`). A job's live view is a snapshot of a running
     * request, so it never enters the catalog — it is shown as the live line
     * of the job it names.
     */
    liveProgress: Map<string, WorkloadLiveProgress>
    initialize: () => Promise<void>
    /** Re-fetch workloads from service (e.g. after state reset / leave cluster). */
    refresh: () => Promise<void>
    cleanup: () => void
}

/**
 * How many live job views the renderer keeps.
 *
 * The map is bounded so a node that never sees a job's terminal event (a peer
 * that joined late, a dropped frame) cannot accumulate views forever. The
 * bound is far above the number of jobs a desktop shows at once.
 */
const MAX_LIVE_VIEWS = 256

let unsubs: Array<() => void> = []

// A single proxy request can emit a burst of upsert/remove pushes (e.g. a
// backend restart removes every catalog entry one by one, and concurrent
// inferences each fire their own lifecycle events). Applying each push as its
// own store commit clones the Map and re-runs every selector per event, which
// fans out into the job list, the connector lines, and node cards. We instead
// buffer pushes and flush them in a single commit on the next animation frame.
type PendingOp =
    | { kind: 'upsert'; workload: Workload }
    | { kind: 'remove'; key: string }
    | { kind: 'live'; key: string; frame: WorkloadLiveProgress }
let pendingOps: PendingOp[] = []
let flushHandle = 0

export const useWorkloadsStore = create<WorkloadsStore>((set, get) => ({
    workloads: new Map(),
    liveProgress: new Map(),

    initialize: async () => {
        const flush = () => {
            flushHandle = 0
            const ops = pendingOps
            pendingOps = []
            if (ops.length === 0) return

            const prev = get().workloads
            const prevLive = get().liveProgress
            // Build the next Maps lazily so a burst that nets no change (e.g. a
            // deep-equal upsert) never triggers a re-render.
            let updated: Map<string, Workload> | null = null
            let updatedLive: Map<string, WorkloadLiveProgress> | null = null
            for (const op of ops) {
                if (op.kind === 'live') {
                    const base = updatedLive ?? prevLive
                    // A job that has already finished has no live line: the
                    // broker retires its live view, so a frame that arrives
                    // after the terminal event must not bring it back.
                    const workload = (updated ?? prev).get(op.key)
                    if (
                        workload &&
                        (workload.state === 'completed' || workload.state === 'failed')
                    ) {
                        continue
                    }
                    const existing = base.get(op.key)
                    const merged = mergeWorkloadLiveProgress(existing, op.frame)
                    if (existing && deepEqual(existing, merged)) continue
                    if (!updatedLive) updatedLive = new Map(prevLive)
                    // Re-insert so the map's order is the order the views were
                    // last updated in; the oldest view is the one that goes
                    // when the map is full.
                    updatedLive.delete(op.key)
                    updatedLive.set(op.key, merged)
                    while (updatedLive.size > MAX_LIVE_VIEWS) {
                        const oldest = updatedLive.keys().next()
                        if (oldest.done) break
                        updatedLive.delete(oldest.value)
                    }
                    continue
                }
                const base = updated ?? prev
                if (op.kind === 'upsert') {
                    const key = workloadKey(op.workload.originatedFrom, op.workload.id)
                    const existing = base.get(key)
                    if (existing && deepEqual(existing, op.workload)) continue
                    if (!updated) updated = new Map(prev)
                    updated.set(key, op.workload)
                    // A finished job's live line goes away with it.
                    if (op.workload.state === 'completed' || op.workload.state === 'failed') {
                        if (!updatedLive) updatedLive = new Map(prevLive)
                        updatedLive.delete(key)
                    }
                } else {
                    if (!base.has(op.key)) continue
                    if (!updated) updated = new Map(prev)
                    updated.delete(op.key)
                    if (!updatedLive) updatedLive = new Map(prevLive)
                    updatedLive.delete(op.key)
                }
            }

            if (updated || updatedLive) {
                set({
                    workloads: updated ?? prev,
                    liveProgress: updatedLive ?? prevLive
                })
            }
        }

        const schedule = () => {
            if (flushHandle) return
            flushHandle = requestAnimationFrame(flush)
        }

        // A `workloads:remove` that lands (and flushes) during the baseline fetch
        // below would be silently undone by rebuilding the map from the baseline
        // snapshot, which still lists the now-retired job. Record such removes so
        // the merge can subtract them — these deltas are newer than the snapshot.
        const removedDuringInit = new Set<string>()
        let initializing = true

        // Subscribe BEFORE fetching the baseline so a `workloads:upsert` that
        // fires during the fetch is buffered rather than dropped. Any such push
        // is applied to the store map by the rAF flush; the baseline merge below
        // then overlays those (newer) entries on top so a live transition is
        // never clobbered by the older snapshot.
        if (window.pairApi) {
            unsubs.push(
                window.pairApi.workloads.onUpsert((workload: Workload) => {
                    pendingOps.push({ kind: 'upsert', workload })
                    schedule()
                }),
                window.pairApi.workloads.onRemove(({ workloadId, originatedFrom }) => {
                    const key = workloadKey(originatedFrom, workloadId)
                    if (initializing) removedDuringInit.add(key)
                    pendingOps.push({ kind: 'remove', key })
                    schedule()
                }),
                // The live view of a running job. It is a snapshot, not a
                // catalog event, so it lands in the live map and never in the
                // catalog.
                window.pairApi.workloads.onProgress((frame: WorkloadLiveProgress) => {
                    const key = workloadKey(frame.originatedFrom, frame.jobId)
                    pendingOps.push({ kind: 'live', key, frame })
                    schedule()
                })
            )
        }

        try {
            const initial = await window.pairApi.workloads.getInitial()
            const map = new Map<string, Workload>()
            for (const [key, workload] of Object.entries(initial)) {
                map.set(key, workload)
            }
            // Subtract jobs a `workloads:remove` retired during the fetch (that
            // delta is newer than the snapshot), before overlaying upserts — so a
            // remove-then-readd of the same key still surfaces the re-add.
            for (const key of removedDuringInit) map.delete(key)
            // Overlay upserts that already landed during the fetch (also newer than
            // the baseline), so seeding never regresses a live transition.
            for (const [key, workload] of get().workloads) {
                map.set(key, workload)
            }
            set({ workloads: map })
        } catch (error) {
            console.error('Failed to initialize workloads store:', error)
        } finally {
            initializing = false
        }
    },

    refresh: async () => {
        try {
            const initial = await window.pairApi.workloads.getInitial()
            const map = new Map<string, Workload>()
            for (const [key, workload] of Object.entries(initial)) {
                map.set(key, workload)
            }
            set({ workloads: map })
        } catch (error) {
            console.error('Failed to refresh workloads store:', error)
        }
    },

    cleanup: () => {
        unsubs.forEach(u => u())
        unsubs = []
        if (flushHandle) {
            cancelAnimationFrame(flushHandle)
            flushHandle = 0
        }
        pendingOps = []
        set({ liveProgress: new Map() })
    }
}))

/**
 * The live view of a running job, if the broker reported one.
 *
 * The view is keyed like the catalog (`originatedFrom` + `id`), because
 * PAIR's job id is a per-proxy counter: two origins can both hold a job "1",
 * and only the origin tells their jobs apart.
 */
export function useWorkloadLiveProgress(workload: Workload): WorkloadLiveProgress | undefined {
    const key = workloadKey(workload.originatedFrom, workload.id)
    return useWorkloadsStore(state => state.liveProgress.get(key))
}

export function useActiveWorkloads(): Workload[] {
    return useWorkloadsStore(
        useShallow(state => {
            const result: Workload[] = []
            for (const w of state.workloads.values()) {
                if (w.state === 'running' || w.state === 'queued' || w.state === 'initializing')
                    result.push(w)
            }
            return result.sort((a, b) => {
                const stateDiff = stateOrder[a.state] - stateOrder[b.state]
                if (stateDiff !== 0) return stateDiff
                if (a.state === 'running' && b.state === 'running')
                    return (b.startedAt ?? 0) - (a.startedAt ?? 0)
                return b.createdAt - a.createdAt
            })
        })
    )
}

export function useAssignedWorkloads(): Workload[] {
    return useWorkloadsStore(
        useShallow(state => {
            const result: Workload[] = []
            for (const w of state.workloads.values()) {
                if (
                    (workloadExecutionNodeId(w) !== null && w.state === 'running') ||
                    w.state === 'queued'
                )
                    result.push(w)
            }
            return result.sort((a, b) => {
                const stateDiff = stateOrder[a.state] - stateOrder[b.state]
                if (stateDiff !== 0) return stateDiff
                return (a.startedAt ?? 0) - (b.startedAt ?? 0)
            })
        })
    )
}

// Number of active (running/queued/initializing) workloads scheduled on a node.
// Returns a primitive so a subscribing node card only re-renders when its own
// count changes, instead of on every workload event across the cluster.
export function useNodeActiveJobCount(nodeId: string): number {
    return useWorkloadsStore(state => {
        let count = 0
        for (const w of state.workloads.values()) {
            if (
                workloadExecutionNodeId(w) === nodeId &&
                (w.state === 'running' || w.state === 'queued' || w.state === 'initializing')
            )
                count++
        }
        return count
    })
}

export function useCompletedWorkloads(): Workload[] {
    return useWorkloadsStore(
        useShallow(state => {
            const result: Workload[] = []
            for (const w of state.workloads.values()) {
                if (w.state === 'completed') result.push(w)
            }
            return result
                .sort((a, b) => (b.completedAt ?? 0) - (a.completedAt ?? 0))
                .slice(0, MAX_HISTORY_ITEMS)
        })
    )
}

export function useFailedWorkloads(): Workload[] {
    return useWorkloadsStore(
        useShallow(state => {
            const result: Workload[] = []
            for (const w of state.workloads.values()) {
                if (w.state === 'failed') result.push(w)
            }
            return result
                .sort((a, b) => (b.completedAt ?? 0) - (a.completedAt ?? 0))
                .slice(0, MAX_HISTORY_ITEMS)
        })
    )
}
