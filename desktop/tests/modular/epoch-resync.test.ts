// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { JsonValue } from '@/shared/types/json'
import type { JsonRpcNotification } from '@/electron/service-bridge/json-rpc-subprocess'

vi.mock('electron', () => ({
    app: { isPackaged: false, getAppPath: () => process.cwd() },
    BrowserWindow: { getAllWindows: () => [] }
}))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

vi.mock('@/shared/utils/log', () => ({
    createStructuredLogger: () => ({
        info: vi.fn(),
        warn: vi.fn(),
        error: vi.fn(),
        verbose: vi.fn()
    })
}))

vi.mock('@/electron/config/ui-config', () => ({ isFirstRun: () => false }))

vi.mock('@/electron/service-bridge/broadcaster', () => ({ emitBridgePush: vi.fn() }))

vi.mock('@/electron/service-bridge/manual-nodes-store', () => ({
    listManualNodeEntries: () => []
}))

vi.mock('@/electron/service-bridge/node-info-poller', () => ({
    startNodeInfoPoller: vi.fn(),
    stopNodeInfoPoller: vi.fn()
}))

import {
    bridgeEpoch,
    getModularSupervisor,
    noteEpoch
} from '@/electron/service-bridge/modular-supervisor'
import { getModularBridgeState } from '@/electron/service-bridge/modular-state'

const EXPECTED_RESYNC_CALLS = [
    'broker:resync',
    'discovery:get-nodes',
    'nodes:get-initial',
    'cluster:get-node-id',
    'engine:get-installed',
    'errors:get-initial',
    'workloads:get-initial',
    'proxy:get-status',
    'lmstudio-proxy:get-status',
    'omlx-proxy:get-status',
    'telemetry/get-status'
]

function invite(overrides: { [key: string]: JsonValue } = {}): { [key: string]: JsonValue } {
    return {
        inviteId: 'invite-resync-1',
        fromNodeId: 'sender.local',
        fromNodeUuid: 'sender-uuid',
        fromNodeName: 'Sender',
        toNodeId: null,
        clusterId: 'cluster-1',
        clusterFriendlyName: 'Home',
        pin: null,
        state: 'pending',
        reason: '',
        createdAt: 1,
        respondedAt: null,
        ...overrides
    }
}

describe('broker epoch tracking and resync', () => {
    let calls: string[]
    let responses: Record<string, () => Promise<JsonValue | undefined>>

    function sendNotification(notification: JsonRpcNotification): void {
        getModularSupervisor()['handleNotification'](notification)
    }

    async function flushMicrotasks(turns = 20): Promise<void> {
        for (let i = 0; i < turns; i++) {
            await Promise.resolve()
        }
    }

    beforeEach(() => {
        calls = []
        responses = {}
        getModularSupervisor()['currentEpoch'] = ''
        getModularSupervisor()['resyncInFlight'] = false
        getModularSupervisor()['brokerReady'] = true
        getModularSupervisor()['brokerHydrationDone'] = true

        getModularSupervisor()['callProcess'] = (_name, method) => {
            calls.push(method)
            const handler = responses[method]
            if (handler) return handler()
            if (method === 'broker:resync') {
                return Promise.resolve({ epoch: bridgeEpoch(), protocol: 2 })
            }
            return Promise.resolve({})
        }
    })

    afterEach(() => {
        for (const entry of getModularBridgeState().getPendingInvites()) {
            getModularBridgeState().prunePendingInvite(entry.inviteId)
        }
        getModularBridgeState().replaceErrors([])
        getModularBridgeState().clearWorkloads()
        vi.useRealTimers()
    })

    it('updates bridgeEpoch and invokes broker:resync plus each snapshot getter once when the epoch changes', async () => {
        expect(bridgeEpoch()).toBe('')
        expect(noteEpoch('')).toBe(false)

        sendNotification({
            source: 'broker',
            method: 'heartbeat',
            params: {},
            epoch: 'epoch-1',
            seq: 1
        })

        expect(bridgeEpoch()).toBe('epoch-1')
        await flushMicrotasks()

        expect(calls).toEqual(EXPECTED_RESYNC_CALLS)
    })

    it('does not resync on a second consecutive frame carrying the same epoch', async () => {
        sendNotification({
            source: 'broker',
            method: 'broker:epoch',
            params: { epoch: 'epoch-same', protocol: 2 },
            epoch: 'epoch-same',
            seq: 1
        })
        await flushMicrotasks()
        expect(calls).toEqual(EXPECTED_RESYNC_CALLS)

        calls.length = 0
        sendNotification({
            source: 'broker',
            method: 'heartbeat',
            params: {},
            epoch: 'epoch-same',
            seq: 2
        })
        await flushMicrotasks()

        expect(calls).toEqual([])
        expect(noteEpoch('epoch-same')).toBe(false)
    })

    it('does not resync on a seq gap or lower seq when the epoch is unchanged, and still applies the frame', async () => {
        sendNotification({
            source: 'broker',
            method: 'heartbeat',
            params: {},
            epoch: 'epoch-gap',
            seq: 1
        })
        await flushMicrotasks()
        calls.length = 0

        sendNotification({
            source: 'broker',
            method: 'errors:update',
            params: [{ id: 'err-gap-50', message: 'gap error', timestamp: 100, severity: 'error' }],
            epoch: 'epoch-gap',
            seq: 50
        })
        await flushMicrotasks()

        expect(calls).toEqual([])
        expect(
            getModularBridgeState()
                .getErrors()
                .map(e => e.id)
        ).toEqual(['err-gap-50'])

        sendNotification({
            source: 'broker',
            method: 'errors:update',
            params: [
                { id: 'err-lower-2', message: 'lower seq error', timestamp: 101, severity: 'error' }
            ],
            epoch: 'epoch-gap',
            seq: 2
        })
        await flushMicrotasks()

        expect(calls).toEqual([])
        expect(
            getModularBridgeState()
                .getErrors()
                .map(e => e.id)
        ).toEqual(['err-lower-2'])
    })

    it('coalesces epoch changes that arrive while a resync burst is already in flight', async () => {
        let releaseResync: (() => void) | undefined
        responses['broker:resync'] = () =>
            new Promise<JsonValue>(resolve => {
                releaseResync = () => resolve({ epoch: 'epoch-a', protocol: 2 })
            })

        expect(noteEpoch('epoch-a')).toBe(true)
        await flushMicrotasks(2)
        expect(calls).toEqual(['broker:resync'])

        expect(noteEpoch('epoch-b')).toBe(true)
        expect(noteEpoch('epoch-c')).toBe(true)
        expect(bridgeEpoch()).toBe('epoch-c')
        await flushMicrotasks(2)

        expect(calls).toEqual(['broker:resync'])

        releaseResync?.()
        await flushMicrotasks()

        expect(calls).toEqual(EXPECTED_RESYNC_CALLS)
        expect(bridgeEpoch()).toBe('epoch-c')

        calls.length = 0
        delete responses['broker:resync']
        expect(noteEpoch('epoch-d')).toBe(true)
        await flushMicrotasks()
        expect(calls).toEqual(EXPECTED_RESYNC_CALLS)
        expect(bridgeEpoch()).toBe('epoch-d')
    })

    it('reconciles a pending invite whose completion push was missed across a broker restart via epoch resync without setInterval', async () => {
        vi.useFakeTimers()
        const setIntervalSpy = vi.spyOn(globalThis, 'setInterval')

        const state = getModularBridgeState()
        sendNotification({
            source: 'broker',
            method: 'cluster:invite-received',
            params: invite(),
            epoch: 'epoch-before-restart',
            seq: 1
        })
        await flushMicrotasks()
        expect(state.getPendingInvites()).toHaveLength(1)
        calls.length = 0
        setIntervalSpy.mockClear()

        responses['nodes:get-initial'] = () =>
            Promise.resolve({
                nodes: [
                    {
                        id: 'sender-uuid',
                        nodeUuid: 'sender-uuid',
                        name: 'Sender',
                        state: 'member',
                        ipAddress: '10.0.0.2',
                        port: 8443,
                        clusterId: 'cluster-1'
                    }
                ]
            })
        responses['discovery:get-nodes'] = () =>
            Promise.resolve({
                nodes: [
                    {
                        id: 'sender-uuid',
                        hostUuid: 'sender-uuid',
                        name: 'Sender',
                        ipAddress: '10.0.0.2',
                        ipAddresses: ['10.0.0.2'],
                        trusted: true,
                        clustered: true,
                        lastSeen: 1000
                    }
                ]
            })

        sendNotification({
            source: 'broker',
            method: 'broker:epoch',
            params: { epoch: 'epoch-after-restart', protocol: 2 },
            epoch: 'epoch-after-restart',
            seq: 1
        })
        await flushMicrotasks()

        expect(bridgeEpoch()).toBe('epoch-after-restart')
        expect(state.getPendingInvites()).toEqual([])
        expect(state.getAvailableNodes().some(n => n.id === 'sender-uuid')).toBe(true)
        expect(setIntervalSpy).not.toHaveBeenCalled()
    })
})
