// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'
import type { getModularBridgeState } from '@/electron/service-bridge/modular-state'

type Supervisor = ReturnType<typeof getModularSupervisor>
type BridgeState = ReturnType<typeof getModularBridgeState>

const mocks = vi.hoisted(() => ({
    state: {
        getSelfId: vi.fn<BridgeState['getSelfId']>(() => 'self-uuid')
    },
    supervisor: {
        hasProcess: vi.fn<Supervisor['hasProcess']>(() => true),
        callProcess: vi.fn<Supervisor['callProcess']>()
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
vi.mock('@/electron/model-hub', () => ({
    getEngineHubModels: vi.fn(),
    lookupEngineHubModel: vi.fn()
}))

import { handleServiceBridgeInvoke } from '@/electron/service-bridge/empty-handlers'

describe('manual-nodes service bridge handlers', () => {
    beforeEach(() => {
        vi.clearAllMocks()
        mocks.supervisor.hasProcess.mockReturnValue(true)
    })

    describe('nodes:manual-list', () => {
        it('calls broker nodes/list and parses nodes correctly', async () => {
            mocks.supervisor.callProcess.mockResolvedValue({
                nodes: [
                    {
                        id: 'node-1',
                        address: '192.168.1.50',
                        name: 'my-peer',
                        kind: 'pair',
                        identityVerification: 'verified',
                        ollama_up: true,
                        ollama_port: 11434,
                        models: ['llama3']
                    },
                    {
                        id: 'node-2',
                        address: 'engine.local',
                        kind: 'engine',
                        lmstudio_up: true,
                        lmstudio_port: 1234
                    }
                ]
            })

            const result = await handleServiceBridgeInvoke('nodes:manual-list', undefined)

            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith('broker', 'nodes/list')
            expect(result).toHaveLength(2)
            expect(result[0]).toEqual({
                id: 'node-1',
                address: '192.168.1.50',
                name: 'my-peer',
                kind: 'pair',
                identityVerification: 'verified',
                ollama_up: true,
                ollama_port: 11434,
                lmstudio_up: false,
                lmstudio_port: 0,
                omlx_up: false,
                omlx_port: 0,
                llamacpp_up: false,
                llamacpp_port: 0,
                node_info_up: false,
                node_info_port: 0,
                tls_enabled: false,
                mtls_required: false,
                models: ['llama3']
            })
            expect(result[1].kind).toBe('engine')
            expect(result[1].lmstudio_up).toBe(true)
        })

        it('returns empty array when supervisor has no broker process', async () => {
            mocks.supervisor.hasProcess.mockReturnValue(false)

            const result = await handleServiceBridgeInvoke('nodes:manual-list', undefined)

            expect(result).toEqual([])
            expect(mocks.supervisor.callProcess).not.toHaveBeenCalled()
        })
    })

    describe('nodes:manual-add', () => {
        it('relays node/add to broker and returns parsed status', async () => {
            mocks.supervisor.callProcess.mockResolvedValue({
                id: 'manual:192.168.1.100',
                address: '192.168.1.100',
                kind: 'pair',
                identityVerification: 'unverified'
            })

            const result = await handleServiceBridgeInvoke('nodes:manual-add', {
                address: '192.168.1.100',
                kind: 'pair'
            })

            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith('broker', 'node/add', {
                address: '192.168.1.100',
                kind: 'pair'
            })
            expect(result.id).toBe('manual:192.168.1.100')
            expect(result.kind).toBe('pair')
            expect(result.identityVerification).toBe('unverified')
        })

        it('relays engine kind with optional fields', async () => {
            mocks.supervisor.callProcess.mockResolvedValue({
                id: 'remote-engine',
                address: '10.0.0.5',
                name: 'remote-engine',
                kind: 'engine'
            })

            const result = await handleServiceBridgeInvoke('nodes:manual-add', {
                address: '10.0.0.5',
                name: 'remote-engine',
                kind: 'engine',
                tls_port: 14319,
                mtls: true
            })

            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith('broker', 'node/add', {
                address: '10.0.0.5',
                name: 'remote-engine',
                kind: 'engine',
                tls_port: 14319,
                mtls: true
            })
            expect(result.kind).toBe('engine')
        })

        it('throws if address is missing', async () => {
            await expect(
                handleServiceBridgeInvoke('nodes:manual-add', {
                    address: '',
                    kind: 'pair'
                })
            ).rejects.toThrow('Address is required')
        })
    })

    describe('nodes:manual-remove', () => {
        it('relays node/remove to broker and returns removed boolean', async () => {
            mocks.supervisor.callProcess.mockResolvedValue({ removed: true })

            const result = await handleServiceBridgeInvoke('nodes:manual-remove', {
                id: 'manual:192.168.1.100'
            })

            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith('broker', 'node/remove', {
                id: 'manual:192.168.1.100'
            })
            expect(result).toEqual({ removed: true })
        })

        it('throws if node id is missing', async () => {
            await expect(
                handleServiceBridgeInvoke('nodes:manual-remove', { id: '' })
            ).rejects.toThrow('Node ID is required')
        })
    })
})
