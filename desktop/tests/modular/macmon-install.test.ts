// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const { invoke } = vi.hoisted(() => ({ invoke: vi.fn() }))
vi.mock('electron', () => ({
    app: { isPackaged: false, getAppPath: () => process.cwd() },
    BrowserWindow: { getAllWindows: () => [] },
    ipcRenderer: { invoke, on: vi.fn(), removeListener: vi.fn() }
}))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { JsonRpcResponseError } from '@/electron/service-bridge/json-rpc-subprocess'
import { handleServiceBridgeInvoke } from '@/electron/service-bridge/empty-handlers'
import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'
import { pairTransport } from '@/preload/api/pair-transport.api'
import type { ServiceBridgeInvokeRequest } from '@/shared/types/service-bridge'
import { createPairApi } from '@/ui/api/pair-api'

const supervisor = getModularSupervisor()

beforeEach(() => {
    vi.spyOn(getModularBridgeState(), 'getSelfId').mockReturnValue('local-1')
    vi.spyOn(supervisor, 'reportError').mockImplementation(() => {})
    invoke.mockImplementation(async (_channel: string, request: ServiceBridgeInvokeRequest) => ({
        success: true,
        data: await handleServiceBridgeInvoke(request.channel, request.payload)
    }))
})

afterEach(() => {
    vi.restoreAllMocks()
    invoke.mockReset()
})

describe('macmon install bridge', () => {
    it('calls engine:install-tool for the local node', async () => {
        const call = vi.spyOn(supervisor, 'callProcess').mockResolvedValue({
            tool: 'macmon',
            installed: true
        })

        await expect(supervisor.installMacmon('local-1')).resolves.toBe('installed')

        expect(call).toHaveBeenCalledExactlyOnceWith(
            'broker',
            'engine:install-tool',
            { tool: 'macmon' },
            21_600_000
        )
        expect(supervisor.reportError).not.toHaveBeenCalled()
    })

    it('calls engine:remote-install-tool for a peer through the renderer API and IPC', async () => {
        const call = vi.spyOn(supervisor, 'callProcess').mockResolvedValue({
            opId: 'op-1',
            result: { tool: 'macmon', installed: true }
        })
        const api = createPairApi(pairTransport)

        await expect(api.nodes.installMacmon('peer-1')).resolves.toBe('installed')

        expect(invoke).toHaveBeenCalledExactlyOnceWith('service-bridge:invoke', {
            channel: 'nodes:install-macmon',
            payload: { nodeId: 'peer-1' }
        })
        expect(call).toHaveBeenCalledExactlyOnceWith(
            'broker',
            'engine:remote-install-tool',
            { node: 'peer-1', tool: 'macmon' },
            21_600_000
        )
        expect(supervisor.reportError).not.toHaveBeenCalled()
    })

    it('treats a non-empty node id as remote when self id is not known', async () => {
        vi.spyOn(getModularBridgeState(), 'getSelfId').mockReturnValue(null)
        const call = vi.spyOn(supervisor, 'callProcess').mockResolvedValue({
            opId: 'op-2',
            result: { tool: 'macmon', installed: true }
        })

        await expect(supervisor.installMacmon('peer-1')).resolves.toBe('installed')

        expect(call).toHaveBeenCalledExactlyOnceWith(
            'broker',
            'engine:remote-install-tool',
            { node: 'peer-1', tool: 'macmon' },
            21_600_000
        )
    })

    it('does not dispatch an empty node id as a remote target', async () => {
        vi.spyOn(getModularBridgeState(), 'getSelfId').mockReturnValue(null)
        const call = vi.spyOn(supervisor, 'callProcess').mockResolvedValue({
            tool: 'macmon',
            installed: true
        })

        await expect(supervisor.installMacmon('')).resolves.toBe('installed')

        expect(call).toHaveBeenCalledExactlyOnceWith(
            'broker',
            'engine:install-tool',
            { tool: 'macmon' },
            21_600_000
        )
    })

    for (const nodeId of ['local-1', 'peer-1']) {
        it(`maps -32010 on ${nodeId} to homebrew-missing without reporting`, async () => {
            vi.spyOn(supervisor, 'callProcess').mockRejectedValue(
                new JsonRpcResponseError('no brew', -32010)
            )

            await expect(supervisor.installMacmon(nodeId)).resolves.toBe('homebrew-missing')

            expect(supervisor.reportError).not.toHaveBeenCalled()
        })

        it(`maps -32011 on ${nodeId} to unsupported-platform without reporting`, async () => {
            vi.spyOn(supervisor, 'callProcess').mockRejectedValue(
                new JsonRpcResponseError('not darwin/arm64', -32011)
            )

            await expect(supervisor.installMacmon(nodeId)).resolves.toBe('unsupported-platform')

            expect(supervisor.reportError).not.toHaveBeenCalled()
        })
    }

    for (const code of [-32012, -32602, -32000]) {
        it(`reports ${code} once with the tool identity and returns failed`, async () => {
            vi.spyOn(supervisor, 'callProcess').mockRejectedValue(
                new JsonRpcResponseError('install rejected', code)
            )

            await expect(supervisor.installMacmon('peer-1')).resolves.toBe('failed')

            expect(supervisor.reportError).toHaveBeenCalledExactlyOnceWith(
                'Failed to install macmon on peer-1: install rejected',
                'error',
                'macmon-install:peer-1',
                {
                    id: 'engine-manager:install-failed:tool:macmon',
                    nodeId: 'peer-1',
                    operation: 'install',
                    action: 'dismiss'
                }
            )
        })
    }

    it('reports a non-RPC failure as failed through the renderer API', async () => {
        vi.spyOn(supervisor, 'callProcess').mockRejectedValue(new Error('broker is not running'))

        await expect(createPairApi(pairTransport).nodes.installMacmon('local-1')).resolves.toBe(
            'failed'
        )

        expect(supervisor.reportError).toHaveBeenCalledExactlyOnceWith(
            'Failed to install macmon on local-1: broker is not running',
            'error',
            'macmon-install:local-1',
            {
                id: 'engine-manager:install-failed:tool:macmon',
                nodeId: 'local-1',
                operation: 'install',
                action: 'dismiss'
            }
        )
    })

    it('does not interpret an untyped rejection as a typed Homebrew error', async () => {
        vi.spyOn(supervisor, 'callProcess').mockRejectedValue({
            code: -32010,
            message: 'no brew'
        })

        await expect(supervisor.installMacmon('peer-1')).resolves.toBe('failed')

        expect(supervisor.reportError).toHaveBeenCalledTimes(1)
    })

    it('converts a synchronous dispatch failure into a failed outcome', async () => {
        vi.spyOn(supervisor, 'callProcess').mockImplementation(() => {
            throw new Error('dispatch failed')
        })

        await expect(supervisor.installMacmon('peer-1')).resolves.toBe('failed')

        expect(supervisor.reportError).toHaveBeenCalledTimes(1)
    })

    it('returns failed without dispatching when the IPC payload is absent', async () => {
        const call = vi.spyOn(supervisor, 'callProcess')

        await expect(
            invoke('service-bridge:invoke', { channel: 'nodes:install-macmon' })
        ).resolves.toEqual({ success: true, data: 'failed' })

        expect(call).not.toHaveBeenCalled()
        expect(supervisor.reportError).not.toHaveBeenCalled()
    })
})
