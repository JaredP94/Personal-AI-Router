// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'
import { parseWorkerStatus, parseWorkers } from '@/electron/service-bridge/modular-state'
import { getModularSupervisor, noteEpoch } from '@/electron/service-bridge/modular-supervisor'
import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import { handleServiceBridgeInvoke } from '@/electron/service-bridge/empty-handlers'
import type { JsonValue } from '@/shared/types/json'
import { emitBridgePush } from '@/electron/service-bridge/broadcaster'

type JsonRow = { [key: string]: JsonValue }

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
vi.mock('@/electron/service-bridge/manual-nodes-store', () => ({ listManualNodeEntries: () => [] }))
vi.mock('@/electron/service-bridge/node-info-poller', () => ({
    startNodeInfoPoller: vi.fn(),
    stopNodeInfoPoller: vi.fn()
}))

const worker: JsonRow = {
    name: 'scanner',
    state: 'running',
    attempts: 0,
    pid: 42,
    restartOnHang: true,
    since: '2026-10-18T12:00:00Z'
}

describe('workers parser', () => {
    it.each(['running', 'degraded', 'restarting', 'exhausted', 'missing'])(
        'accepts %s without changing broker fields',
        state => {
            expect(parseWorkerStatus({ ...worker, state, lastError: 'probe failed' })).toEqual({
                ...worker,
                state,
                lastError: 'probe failed'
            })
        }
    )

    it('drops and logs unknown states without logging the payload', () => {
        const rejected = vi.fn()
        expect(
            parseWorkers({ workers: [worker, { ...worker, state: 'future-state' }] }, rejected)
        ).toEqual([worker])
        expect(rejected).toHaveBeenCalledExactlyOnceWith()
    })

    it.each<JsonRow>([
        { attempts: 'one' },
        { restartOnHang: 'false' },
        { since: null },
        { pid: '42' },
        { lastError: 42 }
    ])('rejects malformed rows %j', patch => {
        expect(parseWorkerStatus({ ...worker, ...patch })).toBeNull()
    })
})

describe('workers broker integration', () => {
    const calls: Array<{ method: string; params: JsonValue | undefined }> = []
    let snapshot: JsonValue = { workers: [worker] }

    beforeEach(async () => {
        vi.restoreAllMocks()
        const supervisor = getModularSupervisor()
        await supervisor.stop()
        supervisor['shuttingDown'] = false
        supervisor['currentEpoch'] = ''
        supervisor['brokerReady'] = true
        supervisor['brokerHydrationDone'] = true
        calls.length = 0
        snapshot = { workers: [worker] }
        supervisor.callProcess = (_name, method, params) => {
            calls.push({ method, params })
            if (method === 'workers:get-status') return Promise.resolve(snapshot)
            if (method === 'workers:restart') return Promise.resolve(worker)
            return Promise.resolve({})
        }
    })

    async function flush() {
        for (let i = 0; i < 100; i++) await Promise.resolve()
    }

    it('hydrates workers on every new epoch', async () => {
        noteEpoch('workers-A')
        await flush()
        expect(getModularBridgeState().getWorkers()).toEqual([worker])
        snapshot = { workers: [{ ...worker, state: 'missing', pid: 0 }] }
        noteEpoch('workers-B')
        await flush()
        expect(calls.filter(call => call.method === 'workers:get-status')).toHaveLength(2)
        expect(getModularBridgeState().getWorkers()[0]?.state).toBe('missing')
    })

    it('replaces the complete list on workers:update, including an empty snapshot', () => {
        const supervisor = getModularSupervisor()
        supervisor['handleNotification']({
            source: 'broker',
            method: 'workers:update',
            params: { workers: [worker] }
        })
        expect(getModularBridgeState().getWorkers()).toEqual([worker])
        supervisor['handleNotification']({
            source: 'broker',
            method: 'workers:update',
            params: { workers: [] }
        })
        expect(getModularBridgeState().getWorkers()).toEqual([])
    })

    it.each(['recovery', 'renderer'])(
        '%s read cannot overwrite a push received during its fetch',
        async path => {
            const supervisor = getModularSupervisor()
            vi.spyOn(supervisor, 'hasProcess').mockReturnValue(true)
            let resolve = (_value: JsonValue | undefined): void => {}
            const held = new Promise<JsonValue | undefined>(done => {
                resolve = done
            })
            const callProcess = supervisor.callProcess
            supervisor.callProcess = (name, method, params) => {
                if (method === 'workers:get-status') {
                    calls.push({ method, params })
                    return held
                }
                return callProcess(name, method, params)
            }
            const read =
                path === 'renderer'
                    ? handleServiceBridgeInvoke('workers:get-status', undefined)
                    : Promise.resolve(noteEpoch('held-recovery'))
            await flush()
            expect(calls.some(call => call.method === 'workers:get-status')).toBe(true)
            const degraded = { ...worker, state: 'degraded' }
            supervisor['handleNotification']({
                source: 'broker',
                method: 'workers:update',
                params: { workers: [degraded] }
            })
            vi.mocked(emitBridgePush).mockClear()
            resolve({ workers: [worker] })
            const result = await read
            await flush()
            expect(getModularBridgeState().getWorkers()).toEqual([degraded])
            if (path === 'renderer') expect(result).toEqual([degraded])
            expect(
                vi
                    .mocked(emitBridgePush)
                    .mock.calls.filter(([channel]) => channel === 'workers:update')
            ).toEqual([])
        }
    )

    it.each(['epoch', 'connection'])(
        'renderer read is fenced across a new %s generation',
        async generation => {
            const supervisor = getModularSupervisor()
            vi.spyOn(supervisor, 'hasProcess').mockReturnValue(true)
            let resolve = (_value: JsonValue | undefined): void => {}
            const held = new Promise<JsonValue | undefined>(done => {
                resolve = done
            })
            const callProcess = supervisor.callProcess
            let first = true
            supervisor.callProcess = (name, method, params) => {
                if (method === 'workers:get-status' && first) {
                    first = false
                    return held
                }
                return callProcess(name, method, params)
            }
            const read = handleServiceBridgeInvoke('workers:get-status', undefined)
            if (generation === 'connection') {
                await supervisor.stop()
                supervisor['shuttingDown'] = false
            }
            snapshot = { workers: [{ ...worker, state: 'exhausted' }] }
            noteEpoch('replacement-epoch')
            await flush()
            expect(getModularBridgeState().getWorkers()[0]?.state).toBe('exhausted')
            vi.mocked(emitBridgePush).mockClear()
            resolve({ workers: [worker] })
            await read
            expect(getModularBridgeState().getWorkers()[0]?.state).toBe('exhausted')
            expect(
                vi
                    .mocked(emitBridgePush)
                    .mock.calls.filter(([channel]) => channel === 'workers:update')
            ).toEqual([])
        }
    )

    it('relays exactly one restart with the requested name', async () => {
        await expect(
            handleServiceBridgeInvoke('workers:restart', { name: 'scanner' })
        ).resolves.toEqual(worker)
        expect(calls).toEqual([{ method: 'workers:restart', params: { name: 'scanner' } }])
    })
})
