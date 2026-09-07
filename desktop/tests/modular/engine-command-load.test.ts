// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
    state: {
        getSelfId: vi.fn(() => 'local-node')
    },
    supervisor: {
        hasProcess: vi.fn(() => true),
        sendProcess: vi.fn(),
        reportError: vi.fn()
    }
}))

vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => mocks.supervisor
}))
vi.mock('@/electron/service-bridge/modular-state', () => ({
    getModularBridgeState: () => mocks.state,
    isProxyEngine: () => false,
    isUpstreamUnreachableError: () => false,
    parseServiceErrors: () => []
}))
vi.mock('@/electron/model-hub', () => ({ getEngineHubModels: vi.fn() }))

import { handleServiceBridgeInvoke } from '@/electron/service-bridge/empty-handlers'

describe('local model load command', () => {
    beforeEach(() => {
        vi.clearAllMocks()
        mocks.state.getSelfId.mockReturnValue('local-node')
        mocks.supervisor.hasProcess.mockReturnValue(true)
    })

    it('observes and attributes an Ollama load rejection to the pending model row', async () => {
        await handleServiceBridgeInvoke('engine:command', {
            command: 'loadModel',
            engineType: 'ollama',
            nodeId: 'local-node',
            model: 'llama3.2'
        })

        expect(mocks.supervisor.sendProcess).toHaveBeenCalledOnce()
        const [name, method, params, onFailure, observeResponse] =
            mocks.supervisor.sendProcess.mock.calls[0]
        expect([name, method, params, observeResponse]).toEqual([
            'broker',
            'engine:action',
            {
                engine: 'ollama',
                action: 'run_model',
                params: { model: 'llama3.2', stream: false }
            },
            true
        ])

        onFailure('timeout awaiting response headers', true)
        expect(mocks.supervisor.reportError).toHaveBeenCalledWith(
            'Failed to load model on ollama: timeout awaiting response headers',
            'error',
            'engine-cmd:load model:ollama',
            {
                nodeId: 'local-node',
                engineType: 'ollama',
                operation: 'load',
                modelName: 'llama3.2'
            }
        )
    })

    it('observes and attributes a non-Ollama load rejection to the pending model row', async () => {
        await handleServiceBridgeInvoke('engine:command', {
            command: 'loadModel',
            engineType: 'omlx',
            nodeId: 'local-node',
            model: 'OsaurusAI--LFM2.5-8B-A1B-MXFP8'
        })

        expect(mocks.supervisor.sendProcess).toHaveBeenCalledOnce()
        const [name, method, params, onFailure, observeResponse] =
            mocks.supervisor.sendProcess.mock.calls[0]
        expect([name, method, params, observeResponse]).toEqual([
            'broker',
            'engine:action',
            {
                engine: 'omlx',
                action: 'load_model',
                params: { model: 'OsaurusAI--LFM2.5-8B-A1B-MXFP8' }
            },
            true
        ])

        onFailure('engine "omlx" has no action "load_model"', true)
        expect(mocks.supervisor.reportError).toHaveBeenCalledWith(
            'Failed to load model on omlx: engine "omlx" has no action "load_model"',
            'error',
            'engine-cmd:load model:omlx',
            {
                nodeId: 'local-node',
                engineType: 'omlx',
                operation: 'load',
                modelName: 'OsaurusAI--LFM2.5-8B-A1B-MXFP8'
            }
        )
    })

    it('observes and attributes an unload rejection to the pending model row', async () => {
        await handleServiceBridgeInvoke('engine:command', {
            command: 'unloadModel',
            engineType: 'omlx',
            nodeId: 'local-node',
            model: 'OsaurusAI--LFM2.5-8B-A1B-MXFP8'
        })

        expect(mocks.supervisor.sendProcess).toHaveBeenCalledOnce()
        const [name, method, params, onFailure, observeResponse] =
            mocks.supervisor.sendProcess.mock.calls[0]
        expect([name, method, params, observeResponse]).toEqual([
            'broker',
            'engine:action',
            {
                engine: 'omlx',
                action: 'unload_model',
                params: { model: 'OsaurusAI--LFM2.5-8B-A1B-MXFP8' }
            },
            true
        ])

        onFailure('failed to unload model', true)
        expect(mocks.supervisor.reportError).toHaveBeenCalledWith(
            'Failed to unload model on omlx: failed to unload model',
            'error',
            'engine-cmd:unload model:omlx',
            {
                nodeId: 'local-node',
                engineType: 'omlx',
                operation: 'unload',
                modelName: 'OsaurusAI--LFM2.5-8B-A1B-MXFP8'
            }
        )
    })
})
