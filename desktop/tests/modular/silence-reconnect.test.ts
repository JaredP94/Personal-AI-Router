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

import { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'

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
        supervisor['stopProcesses'] = originalStopProcesses
        supervisor['isReady'] = false
        supervisor['brokerReady'] = false
        supervisor['onBrokerCrash'] = undefined
        vi.restoreAllMocks()
    })
    it('invalidates readiness, stops once and restarts only once until the new lifetime is ready', async () => {
        const first = attach()
        supervisor['isReady'] = true
        supervisor['brokerReady'] = true
        const crashed = vi.fn()
        supervisor.setOnBrokerCrash(crashed)
        const resyncLifecycle = supervisor['resyncLifecycle']
        first.emit('unresponsive', { source: 'broker' })
        first.emit('unresponsive', { source: 'broker' })
        expect(supervisor.ready).toBe(false)
        expect(supervisor['resyncLifecycle']).toBeGreaterThan(resyncLifecycle)
        await flush()
        expect(crashed).toHaveBeenCalledTimes(1)
        expect(supervisor['stopProcesses']).toHaveBeenCalledTimes(1)
        expect(supervisor.start).toHaveBeenCalledTimes(1)

        const second = attach()
        first.emit('unresponsive', { source: 'broker' }) // superseded child
        second.emit('unresponsive', { source: 'broker' })
        await flush()
        expect(supervisor.start).toHaveBeenCalledTimes(1)
        expect(supervisor['stopProcesses']).toHaveBeenCalledTimes(2)

        const third = attach()
        supervisor['isReady'] = true
        supervisor['brokerReady'] = true
        supervisor['updateReadiness']()
        third.emit('unresponsive', { source: 'broker' })
        await flush()
        expect(supervisor.start).toHaveBeenCalledTimes(2)
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
})
