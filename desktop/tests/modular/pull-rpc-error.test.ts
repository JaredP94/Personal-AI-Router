// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({
    app: { isPackaged: false, getAppPath: () => process.cwd() },
    BrowserWindow: { getAllWindows: () => [] }
}))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'

const supervisor = getModularSupervisor()
const ready = supervisor['brokerReady']

afterEach(() => {
    supervisor['brokerReady'] = ready
    vi.restoreAllMocks()
})

describe('pull RPC error completion', () => {
    it('broker epoch change interrupts canceling downloads and permits retry', () => {
        const state = getModularBridgeState()
        state.setSelfId('epoch-local')
        supervisor.noteEpoch('download-before')
        const opId = state.beginModelPull('ollama', 'epoch-model')
        state.applyLocalEngineProgress({
            engine: 'ollama',
            model: 'epoch-model',
            opId,
            canCancel: true
        })
        state.markModelPullCanceling('epoch-local', 'ollama', 'epoch-model', opId)
        supervisor.noteEpoch('download-after')
        expect(state.isModelPullActive('ollama', 'epoch-model')).toBe(false)
        const replacement = state.beginModelPull('ollama', 'epoch-model')
        state.applyLocalEngineProgress({
            engine: 'ollama',
            model: 'epoch-model',
            opId,
            stage: 'interrupted'
        })
        expect(state.isModelPullActive('ollama', 'epoch-model')).toBe(true)
        state.finishModelPull('ollama', 'epoch-model', replacement)
    })

    for (const target of ['local', 'peer']) {
        it(`clears ${target} pending pull without refreshing inventory on an engine RPC error`, async () => {
            const state = getModularBridgeState()
            state.setSelfId('pull-error-local')
            supervisor['brokerReady'] = true
            const call = vi
                .spyOn(supervisor, 'callProcess')
                .mockRejectedValue(
                    new Error(
                        'Ollama experienced an error while downloading a model: model does not exist'
                    )
                )
            const model = `missing-${target}`
            if (target === 'local') {
                await supervisor.pullModel('ollama', 'ollama', model)
                expect(state.isModelPullActive('ollama', model)).toBe(false)
                expect(call).toHaveBeenCalledExactlyOnceWith(
                    'broker',
                    'engine:action',
                    {
                        engine: 'ollama',
                        action: 'pull_model',
                        params: { name: model },
                        opId: expect.any(String)
                    },
                    expect.any(Number)
                )
            } else {
                await supervisor.pullModelRemote('pull-error-peer', 'ollama', 'ollama', model)
                expect(state.isRemoteModelPullActive('pull-error-peer', 'ollama', model)).toBe(
                    false
                )
                expect(call).toHaveBeenCalledExactlyOnceWith(
                    'broker',
                    'engine:remote-pull-model',
                    { node: 'pull-error-peer', engine: 'ollama', model, opId: expect.any(String) },
                    expect.any(Number)
                )
            }
        })
    }
})
