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

afterEach(() => {
    state.clearWorkloads()
    vi.clearAllMocks()
})

describe('epoch workload snapshots', () => {
    it('releases modification tracking on cancellation and never lets an old token end a new fetch', () => {
        const old = state.beginWorkloadRecovery()
        state.removeWorkloadFromParams({ workloadId: 'removed', originatedFrom: 'origin' })
        expect(old.size).toBe(1)
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
        expect(state.getWorkloads()['origin\u0000live']).toMatchObject({ model: 'live' })
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
        expect(Object.keys(state.getWorkloads())).toEqual(['origin\u0000removed'])
        expect(emitBridgePush).toHaveBeenCalledWith('workloads:remove', {
            workloadId: 'live',
            originatedFrom: 'origin'
        })
        expect(emitBridgePush).toHaveBeenCalledWith(
            'workloads:upsert',
            expect.objectContaining({ id: 'removed', state: 'completed' })
        )
    })
})
