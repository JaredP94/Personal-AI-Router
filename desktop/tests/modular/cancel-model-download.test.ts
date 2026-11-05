// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, expect, it, vi } from 'vitest'
vi.mock('electron', () => ({
    app: { isPackaged: false, getAppPath: () => process.cwd() },
    BrowserWindow: { getAllWindows: () => [] }
}))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))
import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'
import { subscribePush } from '@/electron/service-bridge/push-bus'
import type { EngineProgress } from '@/shared/types/engines'

afterEach(() => vi.restoreAllMocks())

it('does not roll back Canceling when a retry finds the operation already inactive', async () => {
    const state = getModularBridgeState()
    const supervisor = getModularSupervisor()
    state.setSelfId('cancel-retry-local')
    const opId = state.beginModelPull('ollama', 'cancel-retry')
    state.applyLocalEngineProgress({
        engine: 'ollama',
        model: 'cancel-retry',
        opId,
        stage: 'pulling',
        canCancel: true
    })
    const updates: EngineProgress[] = []
    const off = subscribePush(e => {
        if (e.channel === 'engines:progress-changed') updates.push(e.payload)
    })
    const call = vi
        .spyOn(supervisor, 'callProcess')
        .mockResolvedValueOnce({ accepted: true })
        .mockResolvedValueOnce({ accepted: false })
    vi.spyOn(supervisor, 'reportError').mockImplementation(() => {})
    try {
        await supervisor.cancelModelPull(
            'cancel-retry-local',
            'ollama',
            'ollama',
            'cancel-retry',
            opId
        )
        expect(updates.at(-1)?.status).toBe('canceling')
        await supervisor.cancelModelPull(
            'cancel-retry-local',
            'ollama',
            'ollama',
            'cancel-retry',
            opId
        )
        expect(call).toHaveBeenCalledTimes(2)
        expect(updates.at(-1)?.status).toBe('canceling')
        state.applyLocalEngineProgress({
            engine: 'ollama',
            model: 'cancel-retry',
            opId,
            stage: 'done'
        })
        expect(state.isModelPullActive('ollama', 'cancel-retry')).toBe(false)
    } finally {
        off()
    }
})
