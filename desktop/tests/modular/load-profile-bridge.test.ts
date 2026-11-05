// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({ callProcess: vi.fn() }))
vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => mocks
}))
import { handleServiceBridgeInvoke } from '@/electron/service-bridge/empty-handlers'
import { redactSensitiveLogText } from '@/electron/redact-log'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const snapshot = {
    nodeId: 'peer',
    engine: 'lmstudio',
    model: 'model',
    revision: 4,
    loadText: '--gpu max',
    format: 'pair-arguments-v1',
    editable: true,
    loadState: 'reload-required',
    reason: 'Loaded with different options — reload to apply'
}

describe('LM Studio load profiles bridge', () => {
    beforeEach(() => {
        vi.clearAllMocks()
    })
    it('relays the revisioned request to Go for local or paired targets', async () => {
        mocks.callProcess.mockResolvedValue(snapshot)
        const result = await handleServiceBridgeInvoke('engines:apply-load-profile', {
            nodeId: 'peer',
            engine: 'lmstudio',
            model: 'model',
            expectedRevision: 3,
            requestId: 'request',
            loadText: '--gpu max',
            format: 'pair-arguments-v1'
        })
        expect(result).toEqual(snapshot)
        expect(mocks.callProcess).toHaveBeenCalledWith(
            'broker',
            'engine:apply-load-profile',
            expect.objectContaining({
                nodeId: 'peer',
                model: 'model',
                expectedRevision: 3,
                loadText: '--gpu max'
            }),
            20_000
        )
    })
    it('retains authoritative preview errors and literal argv', async () => {
        mocks.callProcess.mockResolvedValue({
            revision: 4,
            loadText: '',
            format: 'pair-arguments-v1',
            load_args: [],
            errors: { loadText: 'Rejected by Go' }
        })
        const result = await handleServiceBridgeInvoke('engines:preview-load-profile', {
            nodeId: 'peer',
            engine: 'lmstudio',
            model: 'model',
            expectedRevision: 4,
            loadText: '--identifier other'
        })
        expect(result.errors).toEqual({ loadText: 'Rejected by Go' })
        expect(result.load_args).toEqual([])
    })
    it('fails closed on missing or malformed service support', async () => {
        mocks.callProcess.mockResolvedValue({})
        await expect(
            handleServiceBridgeInvoke('engines:get-load-profile', {
                nodeId: 'peer',
                engine: 'lmstudio',
                model: 'model'
            })
        ).rejects.toThrow()
    })
    it('keeps private values out of normal and truncated logs', () => {
        for (const frame of [
            JSON.stringify({ loadText: 'private-value', load_args: ['private-value'] }),
            '{"loadText":"private-value'
        ]) {
            expect(redactSensitiveLogText(frame)).not.toContain('private-value')
        }
    })
    it('keeps persistence, peer, broker and desktop waits strictly increasing', () => {
        const source = readFileSync(
            resolve(process.cwd(), '../services/shared/enginesettings/loadprofile.go'),
            'utf8'
        )
        expect(source).toMatch(/LoadProfileBudget = 5 \* time.Second/)
        expect(source).toMatch(/LoadProfileRemoteBudget = 10 \* time.Second/)
        expect(source).toMatch(/LoadProfileRelayBudget = 15 \* time.Second/)
    })
})
