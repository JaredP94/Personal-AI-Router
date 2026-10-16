// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { JsonRpcSubprocess } from '@/electron/service-bridge/json-rpc-subprocess'

const mocks = vi.hoisted(() => ({
    notifyBrokerCrash: vi.fn(),
    notifyBrokerStartupFailure: vi.fn()
}))

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
vi.mock('@/electron/config/ui-config', () => ({
    isFirstRun: () => false,
    getModularLogLevel: () => 'info'
}))
vi.mock('@/electron/service-bridge/broadcaster', () => ({ emitBridgePush: vi.fn() }))
vi.mock('@/electron/service-bridge/manual-nodes-store', () => ({ listManualNodeEntries: () => [] }))
vi.mock('@/electron/service-bridge/node-info-poller', () => ({
    startNodeInfoPoller: vi.fn(),
    stopNodeInfoPoller: vi.fn()
}))
vi.mock('@/electron/connector/broker-crash-notification', () => ({
    notifyBrokerCrash: mocks.notifyBrokerCrash,
    notifyBrokerStartupFailure: mocks.notifyBrokerStartupFailure
}))

import {
    getModularSupervisor,
    ModularStartupTimeoutError
} from '@/electron/service-bridge/modular-supervisor'
import {
    destroyConnector,
    destroyConnectorSync,
    didWeSpawnCli,
    getConnectorError,
    getConnectorStatus,
    initializeConnector,
    restartConnector
} from '@/electron/connector'
import { MODULAR_STARTUP_READY_TIMEOUT_MS } from '@/shared/constants/modular-runtime'

// The real supervisor and the real connector handlers; only process spawning
// and the owned teardown body are replaced, so the supervisor-to-connector
// signalling under test is the production path.
const supervisor = getModularSupervisor()
const originalStopProcesses = supervisor['stopProcesses']
const originalValidateRequiredBinaries = supervisor['validateRequiredBinaries']
let broker: JsonRpcSubprocess | null = null
let releaseStop = (): void => undefined

async function flush(): Promise<void> {
    for (let i = 0; i < 30; i++) await Promise.resolve()
}

function spawnBrokerLifetime(): void {
    const child = new JsonRpcSubprocess('broker', '/isolated-not-started-fixture')
    supervisor['processes'].set('broker', child)
    supervisor['attachChildHandlers'](child)
    supervisor['shuttingDown'] = false
    supervisor['isReady'] = true
    broker = child
}

function brokerBecomesReady(): void {
    supervisor['brokerReady'] = true
    supervisor['updateReadiness']()
}

function currentBroker(): JsonRpcSubprocess {
    if (!broker) throw new Error('no broker lifetime spawned')
    return broker
}

async function connect(): Promise<void> {
    const init = initializeConnector()
    brokerBecomesReady()
    await init
    expect(getConnectorStatus()).toBe('connected')
    expect(didWeSpawnCli()).toBe(true)
}

describe('connector across a silence reconnect', () => {
    beforeEach(() => {
        supervisor['processes'].clear()
        supervisor['silenceRetryAvailable'] = true
        supervisor['silenceRestart'] = null
        supervisor['stopTask'] = null
        supervisor['stopProcesses'] = vi.fn().mockResolvedValue(undefined)
        vi.spyOn(supervisor, 'start').mockImplementation(spawnBrokerLifetime)
        mocks.notifyBrokerCrash.mockClear()
        mocks.notifyBrokerStartupFailure.mockClear()
        releaseStop = (): void => undefined
    })
    afterEach(async () => {
        releaseStop()
        supervisor['stopProcesses'] = vi.fn().mockResolvedValue(undefined)
        await destroyConnector({ force: true })
        await flush()
        supervisor['rejectReadinessWaiters'](new Error('test teardown'))
        supervisor['processes'].clear()
        supervisor['stopProcesses'] = originalStopProcesses
        supervisor['validateRequiredBinaries'] = originalValidateRequiredBinaries
        supervisor['isReady'] = false
        supervisor['brokerReady'] = false
        broker = null
        vi.restoreAllMocks()
    })

    it('stays owner, shows reconnecting, returns to connected, and still tears down on quit', async () => {
        await connect()
        const stop = vi.spyOn(supervisor, 'stop')

        currentBroker().emit('unresponsive', { source: 'broker' })

        expect(getConnectorStatus()).toBe('reconnecting')
        expect(didWeSpawnCli()).toBe(true)
        expect(mocks.notifyBrokerCrash).not.toHaveBeenCalled()
        expect(mocks.notifyBrokerStartupFailure).not.toHaveBeenCalled()

        await flush()
        expect(supervisor.start).toHaveBeenCalledTimes(2)
        brokerBecomesReady()
        expect(getConnectorStatus()).toBe('connected')
        expect(getConnectorError()).toBeUndefined()
        expect(didWeSpawnCli()).toBe(true)

        stop.mockClear()
        await destroyConnector()
        expect(stop).toHaveBeenCalledTimes(1)
        expect(stop).toHaveBeenCalledWith()
        expect(mocks.notifyBrokerCrash).not.toHaveBeenCalled()
    })

    it('quit during the restart window cancels the restart and runs owned teardown', async () => {
        await connect()
        supervisor['stopProcesses'] = vi.fn(
            () =>
                new Promise<void>(resolve => {
                    releaseStop = resolve
                })
        )
        const stop = vi.spyOn(supervisor, 'stop')

        currentBroker().emit('unresponsive', { source: 'broker' })
        expect(getConnectorStatus()).toBe('reconnecting')

        const quitting = destroyConnector()
        releaseStop()
        await quitting
        await flush()

        expect(stop).toHaveBeenCalledWith()
        expect(supervisor.start).toHaveBeenCalledTimes(1)
        expect(getConnectorStatus()).toBe('disconnected')
        expect(didWeSpawnCli()).toBe(false)
    })

    it('reports a genuine failure when no automatic retry is left', async () => {
        await connect()

        currentBroker().emit('unresponsive', { source: 'broker' })
        await flush()
        expect(getConnectorStatus()).toBe('reconnecting')

        // The restarted lifetime goes silent again before it became ready.
        currentBroker().emit('unresponsive', { source: 'broker' })
        await flush()

        expect(supervisor.start).toHaveBeenCalledTimes(2)
        expect(getConnectorStatus()).toBe('disconnected')
        expect(getConnectorError()).toBeDefined()
        expect(didWeSpawnCli()).toBe(false)
        expect(mocks.notifyBrokerStartupFailure).toHaveBeenCalledTimes(1)
    })

    it('reports a genuine failure when the real automatic start() throws', async () => {
        await connect()
        // Run the production start(), which advances the restart ticket before
        // its throwing steps; only the binary check underneath it fails.
        vi.mocked(supervisor.start).mockRestore()
        const validate = vi.fn(() => {
            throw new Error('Required modular binary not found')
        })
        supervisor['validateRequiredBinaries'] = validate

        currentBroker().emit('unresponsive', { source: 'broker' })
        await flush()

        expect(validate).toHaveBeenCalledTimes(1)
        expect(getConnectorStatus()).toBe('disconnected')
        expect(getConnectorError()).toBeDefined()
        expect(didWeSpawnCli()).toBe(false)
        expect(mocks.notifyBrokerStartupFailure).toHaveBeenCalledTimes(1)
        expect(mocks.notifyBrokerCrash).not.toHaveBeenCalled()
    })

    it('reports a genuine failure when the owned stop before the restart fails', async () => {
        await connect()
        supervisor['stopProcesses'] = vi.fn().mockRejectedValue(new Error('teardown failed'))

        currentBroker().emit('unresponsive', { source: 'broker' })
        await flush()

        expect(supervisor.start).toHaveBeenCalledTimes(1)
        expect(getConnectorStatus()).toBe('disconnected')
        expect(didWeSpawnCli()).toBe(false)
        expect(mocks.notifyBrokerStartupFailure).toHaveBeenCalledTimes(1)
    })

    it('a user stop during the restart window wins over a throwing real start()', async () => {
        await connect()
        vi.mocked(supervisor.start).mockRestore()
        const validate = vi.fn(() => {
            throw new Error('Required modular binary not found')
        })
        supervisor['validateRequiredBinaries'] = validate
        supervisor['stopProcesses'] = vi.fn(
            () =>
                new Promise<void>(resolve => {
                    releaseStop = resolve
                })
        )

        currentBroker().emit('unresponsive', { source: 'broker' })
        const stopping = destroyConnector()
        releaseStop()
        await stopping
        await flush()

        expect(validate).not.toHaveBeenCalled()
        expect(getConnectorStatus()).toBe('disconnected')
        expect(getConnectorError()).toBeUndefined()
        expect(didWeSpawnCli()).toBe(false)
        expect(mocks.notifyBrokerCrash).not.toHaveBeenCalled()
        expect(mocks.notifyBrokerStartupFailure).not.toHaveBeenCalled()
    })

    describe('readiness deadline after the automatic restart', () => {
        const timeoutError = new ModularStartupTimeoutError(MODULAR_STARTUP_READY_TIMEOUT_MS)
            .message

        beforeEach(() => {
            vi.useFakeTimers()
        })
        afterEach(() => {
            vi.useRealTimers()
        })

        async function reconnect(): Promise<void> {
            await connect()
            currentBroker().emit('unresponsive', { source: 'broker' })
            await flush()
            expect(supervisor.start).toHaveBeenCalledTimes(2)
            expect(getConnectorStatus()).toBe('reconnecting')
        }

        it('reports a startup failure and keeps ownership when the restarted broker never becomes ready', async () => {
            await reconnect()

            await vi.advanceTimersByTimeAsync(MODULAR_STARTUP_READY_TIMEOUT_MS - 1)
            expect(mocks.notifyBrokerStartupFailure).not.toHaveBeenCalled()
            expect(getConnectorError()).toBeUndefined()

            await vi.advanceTimersByTimeAsync(1)
            expect(getConnectorStatus()).toBe('reconnecting')
            expect(getConnectorError()).toBe(timeoutError)
            expect(didWeSpawnCli()).toBe(true)
            expect(mocks.notifyBrokerStartupFailure).toHaveBeenCalledTimes(1)
            expect(mocks.notifyBrokerStartupFailure).toHaveBeenCalledWith(timeoutError)
            expect(mocks.notifyBrokerCrash).not.toHaveBeenCalled()

            await vi.advanceTimersByTimeAsync(MODULAR_STARTUP_READY_TIMEOUT_MS * 2)
            expect(mocks.notifyBrokerStartupFailure).toHaveBeenCalledTimes(1)

            // A late ready still returns the owner to connected and clears the error.
            brokerBecomesReady()
            expect(getConnectorStatus()).toBe('connected')
            expect(getConnectorError()).toBeUndefined()
            expect(didWeSpawnCli()).toBe(true)
        })

        it('measures the deadline from the automatic start, not from the owned stop before it', async () => {
            await connect()
            supervisor['stopProcesses'] = vi.fn(
                () =>
                    new Promise<void>(resolve => {
                        releaseStop = resolve
                    })
            )

            currentBroker().emit('unresponsive', { source: 'broker' })
            await vi.advanceTimersByTimeAsync(MODULAR_STARTUP_READY_TIMEOUT_MS)
            expect(supervisor.start).toHaveBeenCalledTimes(1)
            releaseStop()
            await flush()
            expect(supervisor.start).toHaveBeenCalledTimes(2)

            await vi.advanceTimersByTimeAsync(MODULAR_STARTUP_READY_TIMEOUT_MS - 1)
            expect(mocks.notifyBrokerStartupFailure).not.toHaveBeenCalled()
            await vi.advanceTimersByTimeAsync(1)
            expect(mocks.notifyBrokerStartupFailure).toHaveBeenCalledTimes(1)
            expect(getConnectorError()).toBe(timeoutError)
        })

        it('stays quiet when the restarted broker becomes ready before the deadline', async () => {
            await reconnect()

            await vi.advanceTimersByTimeAsync(MODULAR_STARTUP_READY_TIMEOUT_MS - 1)
            brokerBecomesReady()
            await vi.advanceTimersByTimeAsync(MODULAR_STARTUP_READY_TIMEOUT_MS * 2)

            expect(getConnectorStatus()).toBe('connected')
            expect(getConnectorError()).toBeUndefined()
            expect(mocks.notifyBrokerStartupFailure).not.toHaveBeenCalled()
            expect(mocks.notifyBrokerCrash).not.toHaveBeenCalled()
        })

        it('a deadline armed before a user restart has no effect on the new start', async () => {
            await reconnect()
            await vi.advanceTimersByTimeAsync(MODULAR_STARTUP_READY_TIMEOUT_MS / 2)
            // Owned teardown empties the process map but, unlike the real one,
            // leaves the pending readiness waiter in place: only the ticket
            // guards the stale deadline here.
            supervisor['stopProcesses'] = vi.fn(async () => {
                supervisor['processes'].clear()
                supervisor['isReady'] = false
            })

            const restarting = restartConnector()
            await flush()
            expect(supervisor.start).toHaveBeenCalledTimes(3)
            expect(getConnectorStatus()).toBe('connecting')

            // The reconnect deadline passes while the user's start is still pending.
            await vi.advanceTimersByTimeAsync(MODULAR_STARTUP_READY_TIMEOUT_MS / 2 + 1)
            expect(getConnectorStatus()).toBe('connecting')
            expect(getConnectorError()).toBeUndefined()
            expect(mocks.notifyBrokerStartupFailure).not.toHaveBeenCalled()

            brokerBecomesReady()
            await restarting
            expect(getConnectorStatus()).toBe('connected')
            expect(mocks.notifyBrokerStartupFailure).not.toHaveBeenCalled()
        })

        it('a deadline armed before quit has no effect after destroyConnector', async () => {
            await reconnect()

            await destroyConnector()
            await vi.advanceTimersByTimeAsync(MODULAR_STARTUP_READY_TIMEOUT_MS * 2)

            expect(getConnectorStatus()).toBe('disconnected')
            expect(getConnectorError()).toBeUndefined()
            expect(didWeSpawnCli()).toBe(false)
            expect(mocks.notifyBrokerStartupFailure).not.toHaveBeenCalled()
            expect(mocks.notifyBrokerCrash).not.toHaveBeenCalled()
        })

        it('a deadline armed before quit has no effect after destroyConnectorSync', async () => {
            await reconnect()

            destroyConnectorSync()
            await vi.advanceTimersByTimeAsync(MODULAR_STARTUP_READY_TIMEOUT_MS * 2)

            expect(getConnectorStatus()).toBe('disconnected')
            expect(getConnectorError()).toBeUndefined()
            expect(didWeSpawnCli()).toBe(false)
            expect(mocks.notifyBrokerStartupFailure).not.toHaveBeenCalled()
            expect(mocks.notifyBrokerCrash).not.toHaveBeenCalled()
        })
    })
})
