// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'
vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))
import { getModularBridgeState } from '@/electron/service-bridge/modular-state'

const peer = {
    hostUuid: 'proven-peer',
    peerRecordId: 'proof:binding',
    identityVerification: 'verified',
    name: 'same-name',
    ipAddress: '192.0.2.5',
    port: 14318,
    trusted: true,
    lastSeen: Date.now() - 3_600_000,
    models: ['peer-model']
}
const preview = {
    ...peer,
    peerRecordId: 'preview:claim',
    identityVerification: 'unverified',
    trusted: false,
    lastSeen: Date.now(),
    models: []
}

describe('independent broker record identities', () => {
    it.each([false, true])('keeps preview separate regardless of snapshot order (%s)', reverse => {
        const state = getModularBridgeState()
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: { nodes: reverse ? [preview, peer] : [peer, preview] }
        })
        const nodes = state.getNodesInitial().nodes
        expect(Object.keys(nodes).sort()).toEqual(['preview:claim', 'proven-peer'])
        expect(nodes['proven-peer']).toMatchObject({
            name: 'same-name',
            identityVerification: 'verified'
        })
        expect(nodes['preview:claim']).toMatchObject({ identityVerification: 'unverified' })
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: { nodes: [peer] }
        })
        expect(Object.keys(state.getNodesInitial().nodes)).toEqual(['proven-peer'])
    })

    it('drops invalid keys, including reserved proven principals and old missing metadata', () => {
        const state = getModularBridgeState()
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    { ...peer, hostUuid: 'preview:claim' },
                    { ...peer, hostUuid: 'manual:claim' },
                    { hostUuid: 'old-peer', name: 'old' },
                    { ...peer, identityVerification: 'unverified' }
                ]
            }
        })
        expect(state.getNodesInitial().nodes).toEqual({})
    })

    it('a proxy event cannot create or resurrect a host identity', () => {
        const state = getModularBridgeState()
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: { nodes: [preview] }
        })
        state.handleNotification({
            source: 'proxy',
            method: 'node/discovered',
            params: {
                id: peer.hostUuid,
                host: peer.name,
                ip: peer.ipAddress,
                port: 11434
            }
        })
        expect(Object.keys(state.getNodesInitial().nodes)).toEqual(['preview:claim'])
        expect(state.getNodeInfoPollTargets()).toEqual([])
    })
})
