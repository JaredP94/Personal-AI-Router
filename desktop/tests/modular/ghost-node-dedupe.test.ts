// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'
vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))
import { getModularBridgeState } from '@/electron/service-bridge/modular-state'

const old = {
    hostUuid: 'old-peer',
    identityVerification: 'verified',
    peerRecordId: 'proof:old',
    name: 'same-name',
    ipAddress: '192.0.2.10',
    port: 14318,
    lastSeen: Date.now() - 3_600_000
}
const current = { ...old, hostUuid: 'new-peer', peerRecordId: 'proof:new', lastSeen: Date.now() }

describe('broker-owned node lifetime', () => {
    it.each([false, true])(
        'does not collapse different keys by address, name or age (%s)',
        reverse => {
            const state = getModularBridgeState()
            state.handleNotification({
                source: 'broker',
                method: 'discovery:nodes-changed',
                params: {
                    nodes: reverse ? [current, old] : [old, current]
                }
            })
            expect(Object.keys(state.getNodesInitial().nodes).sort()).toEqual([
                'new-peer',
                'old-peer'
            ])
            state.handleNotification({
                source: 'broker',
                method: 'discovery:nodes-changed',
                params: { nodes: [current] }
            })
            expect(Object.keys(state.getNodesInitial().nodes)).toEqual(['new-peer'])
        }
    )

    it('withdraws a broker record even when a proxy still reports its engine', () => {
        const state = getModularBridgeState()
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: { nodes: [old] }
        })
        state.handleNotification({
            source: 'proxy',
            method: 'node/discovered',
            params: { id: 'old-peer', port: 11434 }
        })
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: { nodes: [] }
        })
        expect(state.getNodesInitial().nodes).toEqual({})
        state.handleNotification({
            source: 'proxy',
            method: 'node/updated',
            params: { id: 'old-peer', port: 11434 }
        })
        expect(state.getNodesInitial().nodes).toEqual({})
    })
})
