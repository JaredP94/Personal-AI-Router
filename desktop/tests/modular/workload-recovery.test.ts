// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, describe, expect, it, vi } from 'vitest'
import {
    getModularBridgeState,
    parseWorkloadsInitial
} from '@/electron/service-bridge/modular-state'
import { emitBridgePush } from '@/electron/service-bridge/broadcaster'

vi.mock('@/electron/service-bridge/broadcaster', () => ({ emitBridgePush: vi.fn() }))

const state = getModularBridgeState()

function key(originatedFrom: string, engine: string, id: string, runId = ''): string {
    return [originatedFrom, engine, runId, id].join('\u0000')
}

function upsert(id: string, engine: string, runId: string | null = null): void {
    state.upsertWorkloadFromInfo({
        workloadInfo: {
            id,
            engine,
            runId,
            state: 'running',
            model: engine,
            originatedFrom: 'origin',
            createdAt: 1
        }
    })
}

afterEach(() => {
    state.clearWorkloads()
    vi.clearAllMocks()
})

describe('epoch workload snapshots', () => {
    it('releases modification tracking on cancellation and never lets an old token end a new fetch', () => {
        const old = state.beginWorkloadRecovery()
        state.removeWorkloadFromParams({ workloadId: 'removed', originatedFrom: 'origin' })
        state.endWorkloadRecovery(old)
        expect(old.size).toBe(0)

        const fresh = state.beginWorkloadRecovery()
        state.upsertWorkloadFromInfo({
            workloadInfo: {
                id: 'live',
                engine: 'ollama',
                state: 'running',
                model: 'live',
                originatedFrom: 'origin',
                createdAt: 1
            }
        })
        state.endWorkloadRecovery(old, [])
        state.endWorkloadRecovery(fresh, [])
        expect(state.getWorkloads()[key('origin', 'ollama', 'live')]).toMatchObject({
            model: 'live'
        })
        expect(fresh.size).toBe(0)

        const next = state.beginWorkloadRecovery()
        state.endWorkloadRecovery(
            next,
            parseWorkloadsInitial({
                workloads: [
                    {
                        id: 'removed',
                        engine: 'ollama',
                        state: 'completed',
                        model: 'snapshot',
                        originatedFrom: 'origin',
                        createdAt: 1
                    }
                ]
            })
        )
        expect(Object.keys(state.getWorkloads())).toEqual([key('origin', 'ollama', 'removed')])
        expect(emitBridgePush).toHaveBeenCalledWith('workloads:remove', {
            workloadId: 'live',
            originatedFrom: 'origin',
            engine: 'ollama',
            runId: null
        })
        expect(emitBridgePush).toHaveBeenCalledWith(
            'workloads:upsert',
            expect.objectContaining({ id: 'removed', state: 'completed' })
        )
    })
})

describe('job identity', () => {
    // Every proxy numbers its jobs from 1, so an Ollama job "3" and an oMLX
    // job "3" on one node are two jobs, and so are a job and the one a
    // restarted proxy gave the same number.
    it('keeps jobs that share a number on different engines and runs apart', () => {
        upsert('3', 'ollama')
        upsert('3', 'omlx')
        upsert('3', 'omlx', 'run-2')
        expect(Object.keys(state.getWorkloads()).sort()).toEqual(
            [
                key('origin', 'ollama', '3'),
                key('origin', 'omlx', '3'),
                key('origin', 'omlx', '3', 'run-2')
            ].sort()
        )
    })

    // The broker's removal names only (originatedFrom, id) and drops every
    // matching job, as the broker's own store does.
    it('drops every job a broker removal names', () => {
        upsert('3', 'ollama')
        upsert('3', 'omlx')
        upsert('4', 'omlx')
        state.removeWorkloadFromParams({ workloadId: '3', originatedFrom: 'origin' })
        expect(Object.keys(state.getWorkloads())).toEqual([key('origin', 'omlx', '4')])
    })

    // A removal seen while a snapshot is fetched also retires a matching job
    // that only the (older) snapshot holds.
    it('does not let a recovery snapshot bring back a job removed during the fetch', () => {
        const snapshot = state.beginWorkloadRecovery()
        state.removeWorkloadFromParams({ workloadId: '3', originatedFrom: 'origin' })
        state.endWorkloadRecovery(
            snapshot,
            parseWorkloadsInitial({
                workloads: [
                    {
                        id: '3',
                        engine: 'omlx',
                        state: 'running',
                        model: 'm',
                        originatedFrom: 'origin',
                        createdAt: 1
                    },
                    {
                        id: '4',
                        engine: 'omlx',
                        state: 'running',
                        model: 'm',
                        originatedFrom: 'origin',
                        createdAt: 1
                    }
                ]
            })
        )
        expect(Object.keys(state.getWorkloads())).toEqual([key('origin', 'omlx', '4')])
    })
})
