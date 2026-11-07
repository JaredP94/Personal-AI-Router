// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
    state: {
        getSelfId: vi.fn(() => 'local-node'),
        getProxyPort: vi.fn(() => 11434),
        beginLocalEngineOp: vi.fn(),
        failLocalEngineOp: vi.fn()
    },
    supervisor: {
        hasProcess: vi.fn(() => true),
        callProcess: vi.fn(),
        callProxy: vi.fn(),
        sendProcess: vi.fn(),
        reportError: vi.fn(),
        deleteModel: vi.fn()
    }
}))

vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => mocks.supervisor
}))
vi.mock('@/electron/service-bridge/modular-state', () => ({
    getModularBridgeState: () => mocks.state,
    isProxyEngine: (value: string) =>
        value === 'ollama' || value === 'lm-studio' || value === 'omlx',
    isUpstreamUnreachableError: () => false,
    parseServiceErrors: () => []
}))
vi.mock('@/electron/model-hub', () => ({ getEngineHubModels: vi.fn() }))

import { handleServiceBridgeInvoke } from '@/electron/service-bridge/empty-handlers'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { MODULAR_ENGINE_SETTINGS_APPLY_TIMEOUT_MS } from '@/shared/constants/modular-runtime'

const baseline = {
    nodeId: 'local-node',
    engine: 'ollama',
    revision: 4,
    appliedRevision: 4,
    epoch: 'epoch',
    sequence: 1,
    phase: 'idle',
    format: 'pair-arguments-v1',
    settings: { serverPort: 1235, proxyPort: 11434, launchText: 'managed serve' },
    effectiveServerPort: 1235,
    effectiveProxyPort: 11434,
    running: true,
    adopted: false,
    editable: true
}

describe('unified settings bridge', () => {
    beforeEach(() => {
        vi.clearAllMocks()
        mocks.supervisor.callProcess.mockImplementation(async (_name: string, method: string) =>
            method === 'engine:get-settings' ? baseline : { revision: 5, phase: 'succeeded' }
        )
    })

    it('relays llama.cpp catalog queries and preserves service errors', async () => {
        mocks.supervisor.callProcess.mockResolvedValueOnce({
            models: [{ id: 'ggml-org/Small:Q4_K_M', file: 'hash.gguf', size: 42 }]
        })
        const result = await handleServiceBridgeInvoke('engine:search-hub', {
            engineType: 'llamacpp',
            query: 'Small'
        })
        expect(result.models[0]?.name).toBe('ggml-org/Small:Q4_K_M')
        expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
            'broker',
            'engine:model-catalog',
            { query: 'Small' },
            30_000
        )
        mocks.supervisor.callProcess.mockRejectedValueOnce(new Error('not enough disk space'))
        await expect(
            handleServiceBridgeInvoke('engine:search-hub', { engineType: 'llamacpp' })
        ).rejects.toThrow('not enough disk space')
    })

    it('lists local installed cache files without substituting a peer inventory', async () => {
        mocks.supervisor.callProcess.mockResolvedValueOnce({
            models: [
                { id: 'ggml-org/Small:Q4_K_M', file: 'hash.gguf', size: 42, sha256: 'verified' }
            ]
        })
        expect(
            await handleServiceBridgeInvoke('engine:downloaded-models', { nodeId: 'local-node' })
        ).toEqual([
            { id: 'ggml-org/Small:Q4_K_M', file: 'hash.gguf', size: 42, sha256: 'verified' }
        ])
        await expect(
            handleServiceBridgeInvoke('engine:downloaded-models', { nodeId: 'peer' })
        ).rejects.toThrow('not available')
    })

    it('acknowledges local llama.cpp delete only after the owning operation settles', async () => {
        let finish = (): void => {}
        mocks.supervisor.deleteModel.mockImplementation(
            () =>
                new Promise<void>(resolve => {
                    finish = resolve
                })
        )
        let acknowledged = false
        const response = handleServiceBridgeInvoke('engine:command', {
            command: 'deleteModel',
            engineType: 'llamacpp',
            nodeId: 'local-node',
            model: 'ggml-org/Demo:Q4_K_M'
        })
        void response.then(() => {
            acknowledged = true
        })
        await Promise.resolve()
        expect(acknowledged).toBe(false)
        finish()
        await response
        expect(mocks.supervisor.deleteModel).toHaveBeenCalledWith(
            'llamacpp',
            'llamacpp',
            'ggml-org/Demo:Q4_K_M'
        )
    })

    it('updates llama.cpp through atomic install without uninstalling the working tree', async () => {
        await handleServiceBridgeInvoke('engine:command', {
            command: 'update',
            engineType: 'llamacpp',
            nodeId: 'local-node'
        })
        expect(mocks.supervisor.sendProcess).toHaveBeenCalledExactlyOnceWith(
            'broker',
            'engine:install',
            { engine: 'llamacpp', start: true },
            expect.any(Function),
            true
        )
    })

    // Applying settings can stop, rebind, and restart an engine, so it needs
    // the Apply deadline rather than the shorter lifecycle request timeout.
    it('sends one node-owned apply with an end-to-end deadline', async () => {
        const receipt = await handleServiceBridgeInvoke('engines:apply-settings', {
            nodeId: 'local-node',
            engine: 'ollama',
            expectedRevision: 4,
            requestId: 'request',
            settings: { serverPort: 11434, proxyPort: 1235, launchText: 'managed serve' }
        })
        expect(receipt).toEqual({ revision: 5, phase: 'succeeded' })
        expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
            'broker',
            'engine:apply-settings',
            expect.objectContaining({
                engine: 'ollama',
                expectedRevision: 4,
                settings: { serverPort: 11434, proxyPort: 1235, launchText: 'managed serve' }
            }),
            MODULAR_ENGINE_SETTINGS_APPLY_TIMEOUT_MS
        )
        expect(mocks.supervisor.callProxy).not.toHaveBeenCalled()
        expect(mocks.supervisor.callProcess.mock.calls.map(call => call[1])).toEqual([
            'engine:apply-settings'
        ])
    })

    it('addresses a remote node through the same broker relay', async () => {
        await handleServiceBridgeInvoke('engines:apply-settings', {
            nodeId: 'remote-node',
            engine: 'ollama',
            expectedRevision: 4,
            requestId: 'request',
            settings: { ...baseline.settings, proxyPort: 12000 }
        })
        expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
            'broker',
            'engine:apply-settings',
            expect.objectContaining({
                nodeId: 'remote-node',
                settings: { ...baseline.settings, proxyPort: 12000 }
            }),
            MODULAR_ENGINE_SETTINGS_APPLY_TIMEOUT_MS
        )
    })

    it('keeps the desktop Apply deadline outside broker and relay budgets', () => {
        const source = readFileSync(
            resolve(process.cwd(), '../services/shared/enginesettings/timeouts.go'),
            'utf8'
        )
        // Read the actual Go definitions, so this assertion cannot pass against
        // a second, independently maintained relay timeout in TypeScript.
        expect(source).toMatch(/ConfigureBudget\s*=\s*10\*time.Minute \+ 30\*time.Second/)
        expect(source).toMatch(/OperationBudget\s*=\s*ConfigureBudget \+ 30\*time.Second/)
        expect(source).toMatch(/ApplyBudget\s*=\s*OperationBudget \+ time.Minute/)
        expect(source).toMatch(/RelayBudget\s*=\s*ApplyBudget \+ time.Minute/)
        const brokerBudgetMs = (10 * 60 + 30 + 30 + 60) * 1000
        const relayBudgetMs = brokerBudgetMs + 60_000
        expect(MODULAR_ENGINE_SETTINGS_APPLY_TIMEOUT_MS).toBeGreaterThan(relayBudgetMs)
        expect(MODULAR_ENGINE_SETTINGS_APPLY_TIMEOUT_MS).toBe(14 * 60_000)
    })

    it('validates full snapshots and reports unsupported older devices', async () => {
        mocks.supervisor.callProcess.mockResolvedValue({})
        await expect(
            handleServiceBridgeInvoke('engines:get-settings', {
                nodeId: 'remote',
                engine: 'ollama'
            })
        ).rejects.toThrow()
    })
})
