// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { JsonRpcSubprocess } from '@/electron/service-bridge/json-rpc-subprocess'

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

import {
    getModularSupervisor,
    ModularStartupTimeoutError
} from '@/electron/service-bridge/modular-supervisor'
import { MODULAR_STARTUP_READY_TIMEOUT_MS } from '@/shared/constants/modular-runtime'

const supervisor = getModularSupervisor()
const originalStopProcesses = supervisor['stopProcesses']
async function flush(): Promise<void> {
    for (let i = 0; i < 30; i++) await Promise.resolve()
}
function attach(): JsonRpcSubprocess {
    const child = new JsonRpcSubprocess('broker', '/isolated-not-started-fixture')
    supervisor['processes'].set('broker', child)
    supervisor['attachChildHandlers'](child)
    supervisor['shuttingDown'] = false
    return child
}

describe('silence reconnect owner', () => {
    beforeEach(() => {
        supervisor['processes'].clear()
        supervisor['silenceRetryAvailable'] = true
        supervisor['silenceRestart'] = null
        supervisor['stopTask'] = null
        supervisor['stopProcesses'] = vi.fn().mockResolvedValue(undefined)
        vi.spyOn(supervisor, 'start').mockImplementation(() => undefined)
    })
    afterEach(async () => {
        supervisor['processes'].clear()
        await flush()
        supervisor['rejectReadinessWaiters'](new Error('test teardown'))
        supervisor['stopProcesses'] = originalStopProcesses
        supervisor['isReady'] = false
        supervisor['brokerReady'] = false
        supervisor['onBrokerCrash'] = undefined
        supervisor['onBrokerReconnecting'] = undefined
        supervisor['onBrokerReconnectTimeout'] = undefined
        vi.useRealTimers()
        vi.restoreAllMocks()
    })
    it('invalidates readiness, stops once and restarts only once until the new lifetime is ready', async () => {
        const first = attach()
        supervisor['isReady'] = true
        supervisor['brokerReady'] = true
        const crashed = vi.fn()
        const reconnecting = vi.fn()
        supervisor.setOnBrokerCrash(crashed)
        supervisor.setOnBrokerReconnecting(reconnecting)
        const resyncLifecycle = supervisor['resyncLifecycle']
        first.emit('unresponsive', { source: 'broker' })
        first.emit('unresponsive', { source: 'broker' })
        expect(supervisor.ready).toBe(false)
        expect(supervisor['resyncLifecycle']).toBeGreaterThan(resyncLifecycle)
        await flush()
        expect(reconnecting).toHaveBeenCalledTimes(1)
        expect(crashed).not.toHaveBeenCalled()
        expect(supervisor['stopProcesses']).toHaveBeenCalledTimes(1)
        expect(supervisor.start).toHaveBeenCalledTimes(1)

        const second = attach()
        first.emit('unresponsive', { source: 'broker' }) // superseded child
        second.emit('unresponsive', { source: 'broker' })
        await flush()
        expect(supervisor.start).toHaveBeenCalledTimes(1)
        expect(supervisor['stopProcesses']).toHaveBeenCalledTimes(2)
        // No retry left: a genuine failure, reported as a crash.
        expect(crashed).toHaveBeenCalledTimes(1)
        expect(reconnecting).toHaveBeenCalledTimes(1)

        const third = attach()
        supervisor['isReady'] = true
        supervisor['brokerReady'] = true
        supervisor['updateReadiness']()
        third.emit('unresponsive', { source: 'broker' })
        await flush()
        expect(supervisor.start).toHaveBeenCalledTimes(2)
        expect(reconnecting).toHaveBeenCalledTimes(2)
    })
    it('a user stop cancels an in-flight automatic restart and shares the owned stop', async () => {
        let release = (): void => undefined
        supervisor['stopProcesses'] = vi.fn(
            () =>
                new Promise<void>(resolve => {
                    release = resolve
                })
        )
        const first = attach()
        first.emit('unresponsive', { source: 'broker' })
        const stopping = supervisor.stop()
        release()
        await stopping
        await flush()
        expect(supervisor.start).not.toHaveBeenCalled()
        expect(supervisor['stopProcesses']).toHaveBeenCalledTimes(1)
    })
    it('arms one readiness deadline per automatic start and ignores it once the ticket moves on', async () => {
        vi.useFakeTimers()
        const spawn = (): void => {
            attach()
            supervisor['isReady'] = true
        }
        vi.mocked(supervisor.start).mockImplementation(spawn)
        const timedOut = vi.fn()
        supervisor.setOnBrokerReconnectTimeout(timedOut)

        spawn()
        supervisor['brokerReady'] = true
        supervisor['updateReadiness']()
        supervisor['processes'].get('broker')?.emit('unresponsive', { source: 'broker' })
        await flush()
        expect(supervisor.start).toHaveBeenCalledTimes(1)
        await vi.advanceTimersByTimeAsync(MODULAR_STARTUP_READY_TIMEOUT_MS)
        expect(timedOut).toHaveBeenCalledTimes(1)
        expect(timedOut.mock.calls[0]?.[0]).toBeInstanceOf(ModularStartupTimeoutError)

        // A fresh lifetime earns a new retry; a user stop after its automatic
        // start leaves that start's deadline stale even if its waiter survives.
        supervisor['brokerReady'] = true
        supervisor['updateReadiness']()
        supervisor['processes'].get('broker')?.emit('unresponsive', { source: 'broker' })
        await flush()
        expect(supervisor.start).toHaveBeenCalledTimes(2)
        await supervisor.stop()
        await vi.advanceTimersByTimeAsync(MODULAR_STARTUP_READY_TIMEOUT_MS * 2)
        expect(timedOut).toHaveBeenCalledTimes(1)
    })

    it('reports a crash on protocol-mismatch without attempting a reconnect or consuming silenceRetryAvailable', async () => {
        const broker = attach()
        supervisor['isReady'] = true
        supervisor['brokerReady'] = true
        const crashed = vi.fn()
        const reconnecting = vi.fn()
        supervisor.setOnBrokerCrash(crashed)
        supervisor.setOnBrokerReconnecting(reconnecting)

        broker.emit('protocol-mismatch', {
            source: 'broker',
            reason: 'notification without epoch/seq (method "echo")'
        })
        await flush()

        expect(crashed).toHaveBeenCalledTimes(1)
        expect(crashed).toHaveBeenCalledWith(
            expect.objectContaining({
                code: null,
                reason: expect.stringContaining('notification without epoch/seq')
            })
        )
        expect(reconnecting).not.toHaveBeenCalled()
        expect(supervisor.start).not.toHaveBeenCalled()
        expect(supervisor['silenceRetryAvailable']).toBe(true)
        expect(supervisor['stopProcesses']).toHaveBeenCalledTimes(1)
    })
})
