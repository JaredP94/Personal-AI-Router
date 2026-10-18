// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { JsonValue } from '@/shared/types/json'
import {
    JsonRpcResponseError,
    JsonRpcSubprocess,
    type JsonRpcNotification
} from '@/electron/service-bridge/json-rpc-subprocess'
import { parseInvite } from '@/electron/service-bridge/cluster-json'

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

    async function flushMicrotasks(turns = 100): Promise<void> {
        for (let i = 0; i < turns; i++) {
            await Promise.resolve()
        }
    }

    beforeEach(async () => {
        await getModularSupervisor().stop()
        getModularSupervisor()['shuttingDown'] = false
        calls = []
        responses = {}
        getModularSupervisor()['currentEpoch'] = ''
        getModularSupervisor()['brokerReady'] = true
        getModularSupervisor()['brokerHydrationDone'] = true

        getModularSupervisor()['callProcess'] = (_name, method) => {
            calls.push(method)
            const handler = responses[method]
            if (handler) return handler()
            if (method === 'cluster:invite-status') return Promise.resolve(invite())
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

    function deferred() {
        let resolveReply: (value: JsonValue) => void = () => {
            throw new Error('not initialized')
        }
        const promise = new Promise<JsonValue>(resolve => {
            resolveReply = resolve
        })
        return { promise, resolve: (value: JsonValue) => resolveReply(value) }
    }

    function job(id: string, state = 'running', model = 'old'): JsonValue {
        return { id, engine: 'ollama', state, model, originatedFrom: 'origin', createdAt: 1 }
    }

    it('runs a serialized latest-epoch follow-up when B arrives during the final A getter', async () => {
        const finalA = deferred()
        responses['telemetry/get-status'] = () => finalA.promise
        responses['errors:get-initial'] = () =>
            Promise.resolve([
                { id: bridgeEpoch(), message: 'snapshot', timestamp: 1, severity: 'error' }
            ])
        noteEpoch('A')
        await flushMicrotasks()
        expect(calls.at(-1)).toBe('telemetry/get-status')
        expect(
            getModularBridgeState()
                .getErrors()
                .map(e => e.id)
        ).toEqual(['A'])
        noteEpoch('B')
        expect(calls.filter(m => m === 'broker:resync')).toHaveLength(1)
        delete responses['telemetry/get-status']
        finalA.resolve({})
        await flushMicrotasks()
        expect(calls.filter(m => m === 'broker:resync')).toHaveLength(2)
        expect(
            getModularBridgeState()
                .getErrors()
                .map(e => e.id)
        ).toEqual(['B'])
        noteEpoch('B')
        await flushMicrotasks()
        expect(calls.filter(m => m === 'broker:resync')).toHaveLength(2)
    })

    it.each([
        'broker:resync',
        'nodes:get-initial',
        'engine:get-installed',
        'proxy:get-status',
        'telemetry/get-status'
    ])('retries a failed %s once on later same-epoch traffic, without spinning', async method => {
        responses[method] = () => Promise.reject(new Error('temporarily unavailable'))
        responses['errors:get-initial'] = () =>
            Promise.resolve([
                { id: 'recovered', message: 'snapshot', timestamp: 1, severity: 'error' }
            ])
        noteEpoch('retry')
        await flushMicrotasks()
        expect(calls.filter(m => m === method)).toHaveLength(1)
        if (method === 'broker:resync') expect(calls).toEqual(['broker:resync'])
        delete responses[method]
        expect(noteEpoch('retry')).toBe(false)
        await flushMicrotasks()
        expect(calls.filter(m => m === method)).toHaveLength(2)
        expect(
            getModularBridgeState()
                .getErrors()
                .map(e => e.id)
        ).toEqual(['recovered'])
        const count = calls.length
        noteEpoch('retry')
        await flushMicrotasks()
        expect(calls).toHaveLength(count)
    })

    it.each(['canceled', 'expired', 'failed', 'declined', 'paired', 'unknown'])(
        'prunes a missed %s invite without a new member',
        async status => {
            const state = getModularBridgeState()
            state.addPendingInvite(parseInvite(invite()))
            responses['cluster:invite-status'] = () =>
                status === 'unknown'
                    ? Promise.reject(new JsonRpcResponseError('opaque error', -32001))
                    : Promise.resolve(invite({ state: status }))
            noteEpoch('invite-terminal')
            await flushMicrotasks()
            expect(state.getPendingInvites()).toEqual([])
            expect(calls.filter(m => m === 'cluster:invite-status')).toHaveLength(1)
            noteEpoch('invite-terminal')
            await flushMicrotasks()
            expect(calls.filter(m => m === 'cluster:invite-status')).toHaveLength(1)
        }
    )

    it('retains pending and transient-error invites, retrying transient failure on the next frame', async () => {
        const state = getModularBridgeState()
        state.addPendingInvite(parseInvite(invite()))
        responses['cluster:invite-status'] = () =>
            Promise.reject(new Error('-32001: not a typed response'))
        noteEpoch('invite-retry')
        await flushMicrotasks()
        expect(state.getPendingInvites()).toHaveLength(1)
        expect(calls.filter(m => m === 'cluster:invite-status')).toHaveLength(1)
        responses['cluster:invite-status'] = () => Promise.resolve(invite())
        noteEpoch('invite-retry')
        await flushMicrotasks()
        expect(state.getPendingInvites()).toHaveLength(1)
        expect(calls.filter(m => m === 'cluster:invite-status')).toHaveLength(2)
        noteEpoch('invite-retry')
        await flushMicrotasks()
        expect(calls.filter(m => m === 'cluster:invite-status')).toHaveLength(2)
    })

    it('ignores a deferred getter from stop ENTRY, before engine teardown finishes', async () => {
        const reply = deferred()
        const teardown = deferred()
        responses['errors:get-initial'] = () => reply.promise
        const supervisor = getModularSupervisor()
        const child = new JsonRpcSubprocess('broker', process.execPath)
        child.call = () => teardown.promise
        child.stop = () => Promise.resolve()
        supervisor['processes'].set('broker', child)
        noteEpoch('old')
        await flushMicrotasks()
        expect(calls.at(-1)).toBe('errors:get-initial')
        const stopped = supervisor.stop()
        reply.resolve([{ id: 'stale', message: 'stale', timestamp: 1, severity: 'error' }])
        await flushMicrotasks()
        const errorsDuringStop = getModularBridgeState().getErrors()
        noteEpoch('during-stop')
        await flushMicrotasks()
        const lastCallDuringStop = calls.at(-1)
        teardown.resolve({})
        await stopped
        expect(errorsDuringStop).toEqual([])
        expect(lastCallDuringStop).toBe('errors:get-initial')
    })

    it('invalidates broker exit replies and keeps a new pass owned when the old finalizer unwinds', async () => {
        const oldReply = deferred()
        const newReply = deferred()
        const supervisor = getModularSupervisor()
        const child = new JsonRpcSubprocess('broker', process.execPath)
        supervisor['processes'].set('broker', child)
        supervisor['attachChildHandlers'](child)
        responses['errors:get-initial'] = () => oldReply.promise
        noteEpoch('old')
        await flushMicrotasks()
        child.emit('exit', { source: 'broker', code: 1 })
        responses['errors:get-initial'] = () => newReply.promise
        noteEpoch('new')
        await flushMicrotasks()
        const count = calls.length
        oldReply.resolve([{ id: 'stale', message: 'stale', timestamp: 1, severity: 'error' }])
        await flushMicrotasks()
        expect(getModularBridgeState().getErrors()).toEqual([])
        noteEpoch('new')
        await flushMicrotasks()
        expect(calls).toHaveLength(count)
        newReply.resolve([{ id: 'fresh', message: 'fresh', timestamp: 1, severity: 'error' }])
        await flushMicrotasks()
        expect(
            getModularBridgeState()
                .getErrors()
                .map(e => e.id)
        ).toEqual(['fresh'])
        expect(calls.filter(m => m === 'broker:resync')).toHaveLength(2)
    })

    it('invalidates a deferred reply at start entry even if startup is already marked ready', async () => {
        const reply = deferred()
        const supervisor = getModularSupervisor()
        responses['errors:get-initial'] = () => reply.promise
        noteEpoch('before-start')
        await flushMicrotasks()
        expect(calls.at(-1)).toBe('errors:get-initial')
        supervisor['isReady'] = true
        supervisor.start()
        reply.resolve([{ id: 'stale', message: 'stale', timestamp: 1, severity: 'error' }])
        await flushMicrotasks()
        expect(getModularBridgeState().getErrors()).toEqual([])
        expect(calls.at(-1)).toBe('errors:get-initial')
    })

    it('does not apply an old-epoch getter when a newer epoch arrives before its reply', async () => {
        const oldReply = deferred()
        const freshReply = deferred()
        responses['errors:get-initial'] = () => oldReply.promise
        noteEpoch('old')
        await flushMicrotasks()
        expect(calls.at(-1)).toBe('errors:get-initial')
        noteEpoch('fresh')
        responses['errors:get-initial'] = () => freshReply.promise
        oldReply.resolve([{ id: 'stale', message: 'stale', timestamp: 1, severity: 'error' }])
        await flushMicrotasks()
        expect(getModularBridgeState().getErrors()).toEqual([])
        expect(calls.filter(m => m === 'broker:resync')).toHaveLength(2)
        freshReply.resolve([{ id: 'fresh', message: 'fresh', timestamp: 1, severity: 'error' }])
        await flushMicrotasks()
        expect(
            getModularBridgeState()
                .getErrors()
                .map(e => e.id)
        ).toEqual(['fresh'])
        expect(calls.filter(m => m === 'workloads:get-initial')).toHaveLength(1)
    })

    it('does not prune a fresh cached invite from a previous lifetime status response', async () => {
        const reply = deferred()
        const state = getModularBridgeState()
        state.addPendingInvite(parseInvite(invite()))
        responses['cluster:invite-status'] = () => reply.promise
        noteEpoch('old')
        await flushMicrotasks()
        expect(calls.at(-1)).toBe('cluster:invite-status')
        await getModularSupervisor().stop()
        state.addPendingInvite(parseInvite(invite({ createdAt: 2 })))
        reply.resolve(invite({ state: 'expired' }))
        await flushMicrotasks()
        expect(state.getPendingInvites()).toMatchObject([{ createdAt: 2 }])
    })

    it('replaces stale workload rows/removes absent jobs but preserves upserts and removal tombstones during fetch', async () => {
        const reply = deferred()
        const state = getModularBridgeState()
        for (const id of ['terminal', 'absent', 'upsert', 'removed'])
            state.upsertWorkloadFromInfo({ workloadInfo: job(id) })
        responses['workloads:get-initial'] = () => reply.promise
        noteEpoch('workloads')
        await flushMicrotasks()
        expect(calls.at(-1)).toBe('workloads:get-initial')
        state.upsertWorkloadFromInfo({ workloadInfo: job('upsert', 'running', 'new-push') })
        state.removeWorkloadFromParams({ workloadId: 'removed', originatedFrom: 'origin' })
        reply.resolve({
            workloads: ['terminal', 'upsert', 'removed'].map(id => job(id, 'completed', 'snapshot'))
        })
        await flushMicrotasks()
        const workloads = state.getWorkloads()
        expect(workloads['origin\u0000terminal']).toMatchObject({
            state: 'completed',
            model: 'snapshot'
        })
        expect(workloads['origin\u0000absent']).toBeUndefined()
        expect(workloads['origin\u0000upsert']).toMatchObject({
            state: 'running',
            model: 'new-push'
        })
        expect(workloads['origin\u0000removed']).toBeUndefined()
        expect(calls.filter(m => m === 'workloads:get-initial')).toHaveLength(1)
        delete responses['workloads:get-initial']
        responses['workloads:get-initial'] = () =>
            Promise.resolve({ workloads: [job('removed', 'completed')] })
        noteEpoch('workloads-next')
        await flushMicrotasks()
        expect(state.getWorkloads()['origin\u0000removed']).toMatchObject({ state: 'completed' })
        expect(state.getWorkloads()['origin\u0000upsert']).toBeUndefined()
    })

    it('updates bridgeEpoch and invokes broker:resync plus each snapshot getter once when the epoch changes', async () => {
        expect(bridgeEpoch()).toBe('')
        expect(noteEpoch('')).toBe(false)

        sendNotification({
            source: 'broker',
            method: 'heartbeat',
            params: {},
            epoch: 'epoch-1',
            seq: 1n
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
            seq: 1n
        })
        await flushMicrotasks()
        expect(calls).toEqual(EXPECTED_RESYNC_CALLS)

        calls.length = 0
        sendNotification({
            source: 'broker',
            method: 'heartbeat',
            params: {},
            epoch: 'epoch-same',
            seq: 2n
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
            seq: 1n
        })
        await flushMicrotasks()
        calls.length = 0

        sendNotification({
            source: 'broker',
            method: 'errors:update',
            params: [{ id: 'err-gap-50', message: 'gap error', timestamp: 100, severity: 'error' }],
            epoch: 'epoch-gap',
            seq: 50n
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
            seq: 2n
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
        responses['broker:resync'] = () => {
            if (calls.filter(method => method === 'broker:resync').length > 1) {
                return Promise.resolve({ epoch: bridgeEpoch(), protocol: 2 })
            }
            return new Promise<JsonValue>(resolve => {
                releaseResync = () => resolve({ epoch: 'epoch-a', protocol: 2 })
            })
        }

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

        expect(calls).toEqual(['broker:resync', ...EXPECTED_RESYNC_CALLS])
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
            seq: 1n
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
            seq: 1n
        })
        await flushMicrotasks()

        expect(bridgeEpoch()).toBe('epoch-after-restart')
        expect(state.getPendingInvites()).toEqual([])
        expect(state.getAvailableNodes().some(n => n.id === 'sender-uuid')).toBe(true)
        expect(setIntervalSpy).not.toHaveBeenCalled()
    })
})
