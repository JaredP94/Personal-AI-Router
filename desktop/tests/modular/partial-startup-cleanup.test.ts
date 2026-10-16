// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * A start() that fails halfway through the spawn loop must not leave the
 * children it already spawned running without an owner.
 *
 * The connector reports such a failure and drops ownership of the tree, so
 * nothing else will ever stop those processes and the next start spawns
 * duplicates beside them. The seam here replaces only (a) the list of
 * Electron-owned definitions, so there are two children to spawn, and
 * (b) JsonRpcSubprocess.start, so nothing is really spawned and the surviving
 * children are exactly the instances the supervisor created and tracked.
 */

import fs from 'fs'
import os from 'os'
import path from 'path'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { JsonRpcSubprocess } from '@/electron/service-bridge/json-rpc-subprocess'
import {
    MODULAR_RUNTIME_BINARIES,
    modularBinaryFileName
} from '@/shared/constants/modular-binaries'
import { currentPlatform } from '@/shared/utils/platform'
import { startNodeInfoPoller } from '@/electron/service-bridge/node-info-poller'

const mocks = vi.hoisted(() => ({
    appDir: '',
    notifyBrokerCrash: vi.fn(),
    notifyBrokerStartupFailure: vi.fn()
}))

vi.mock('electron', () => ({
    app: { isPackaged: false, getAppPath: () => mocks.appDir },
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

import { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'
import {
    destroyConnector,
    didWeSpawnCli,
    getConnectorError,
    getConnectorStatus,
    initializeConnector
} from '@/electron/connector'

const supervisor = getModularSupervisor()
const originalStopProcesses = supervisor['stopProcesses']
const originalElectronOwnedDefinitions = supervisor['electronOwnedDefinitions']

/**
 * One Electron-owned child as start() sees it: the instance the supervisor
 * constructed, plus the shutdown graces it was asked for. An empty `graces` means
 * nothing ever asked that instance to stop.
 */
interface SpawnRecord {
    child: JsonRpcSubprocess
    graces: (number | undefined)[]
    pending: Promise<void>
    release: () => void
}

let spawned: SpawnRecord[] = []
let failSpawnName: string | null = 'scanner'
let holdTeardown = false
const tmpDirs: string[] = []

/**
 * Two Electron-owned definitions instead of production's one (the broker), so a
 * later child can fail after an earlier one is already live. The scanner is the
 * real scanner entry, flipped to the Electron owner for this test only.
 */
function electronOwnedFixture(): typeof MODULAR_RUNTIME_BINARIES {
    const scanner = MODULAR_RUNTIME_BINARIES.find(binary => binary.processName === 'scanner')
    const broker = MODULAR_RUNTIME_BINARIES.find(binary => binary.processName === 'broker')
    if (!scanner || !broker) throw new Error('fixture needs the scanner and broker entries')
    return [broker, { ...scanner, launchOwner: 'electron' }]
}

/**
 * Replace the process launch and nothing else: the instances recorded here are
 * the ones the production spawn loop built, tracked, and attached handlers to.
 * `failSpawnName` fails at that child's own start, which is where a real spawn
 * failure surfaces.
 */
function mockProcessLaunch(): void {
    vi.spyOn(JsonRpcSubprocess.prototype, 'start').mockImplementation(function (
        this: JsonRpcSubprocess
    ) {
        let release = (): void => undefined
        const pending = new Promise<void>(resolve => {
            release = resolve
        })
        const record: SpawnRecord = { child: this, graces: [], pending, release }
        this.stop = async (graceMs?: number): Promise<void> => {
            record.graces.push(graceMs)
            if (holdTeardown) await pending
        }
        spawned.push(record)
        if (failSpawnName === this.name) {
            throw new Error(`spawn failed for ${this.name}`)
        }
    })
}

function releaseTeardowns(): void {
    for (const record of spawned) record.release()
}

function markBrokerReady(): void {
    supervisor['brokerReady'] = true
    supervisor['updateReadiness']()
}

async function flush(): Promise<void> {
    for (let index = 0; index < 30; index++) await Promise.resolve()
}

function createFixtureBinDir(): string {
    const appDir = fs.mkdtempSync(path.join(os.tmpdir(), 'pair-partial-start-'))
    tmpDirs.push(appDir)
    const binDir = path.join(appDir, 'cli-bin')
    fs.mkdirSync(binDir)
    // Every binary the supervisor validates must "exist" so the spawn loop reaches its children
    // rather than skipping them; none is executed, the launch itself is mocked.
    for (const definition of MODULAR_RUNTIME_BINARIES) {
        fs.writeFileSync(
            path.join(binDir, modularBinaryFileName(definition.baseName, currentPlatform())),
            ''
        )
    }
    mocks.appDir = appDir
    return binDir
}

describe('partial start cleanup', () => {
    beforeEach(() => {
        spawned = []
        failSpawnName = 'scanner'
        holdTeardown = false
        createFixtureBinDir()
        supervisor['processes'].clear()
        supervisor['isReady'] = false
        supervisor['brokerReady'] = false
        supervisor['shuttingDown'] = false
        supervisor['silenceRestart'] = null
        supervisor['stopTask'] = null
        supervisor['stopProcesses'] = originalStopProcesses
        supervisor['electronOwnedDefinitions'] = electronOwnedFixture
        mocks.notifyBrokerCrash.mockClear()
        mocks.notifyBrokerStartupFailure.mockClear()
        mockProcessLaunch()
    })

    afterEach(async () => {
        holdTeardown = false
        releaseTeardowns()
        await destroyConnector({ force: true })
        await flush()
        supervisor['processes'].clear()
        supervisor['rejectReadinessWaiters'](new Error('test teardown'))
        supervisor['electronOwnedDefinitions'] = originalElectronOwnedDefinitions
        supervisor['stopProcesses'] = originalStopProcesses
        supervisor['isReady'] = false
        supervisor['brokerReady'] = false
        vi.useRealTimers()
        vi.restoreAllMocks()
        for (const dir of tmpDirs.splice(0)) {
            fs.rmSync(dir, { recursive: true, force: true })
        }
        mocks.appDir = ''
    })

    it('stops the children a failed start spawned instead of leaving them running unowned', async () => {
        await expect(initializeConnector()).rejects.toThrow('spawn failed for scanner')
        await flush()

        // The attempt really got past the broker before it failed.
        expect(spawned.map(record => record.child.name)).toEqual(['broker', 'scanner'])
        // Reverse spawn order, broker on its long engine-shutdown grace, every
        // instance the failed attempt created.
        expect(spawned.map(record => record.graces)).toEqual([[15_000], [undefined]])
        expect(supervisor['processes'].size).toBe(0)
        expect(supervisor.hasProcess('broker')).toBe(false)
        expect(supervisor.hasProcess('scanner')).toBe(false)

        // Unambiguously not ready, and nothing left waiting on it.
        expect(supervisor.ready).toBe(false)
        await expect(supervisor.waitUntilReady(200)).rejects.toThrow(/is not running/)
        expect(supervisor['readinessWaiters'].size).toBe(0)
        expect(vi.mocked(startNodeInfoPoller)).not.toHaveBeenCalled()

        // One report for one failed start: the connector's, not a crash.
        expect(getConnectorStatus()).toBe('disconnected')
        expect(didWeSpawnCli()).toBe(false)
        expect(getConnectorError()).toBe('spawn failed for scanner')
        expect(mocks.notifyBrokerStartupFailure).toHaveBeenCalledTimes(1)
        expect(mocks.notifyBrokerCrash).not.toHaveBeenCalled()
    })

    it('treats the exits of the reaped children as a deliberate shutdown', async () => {
        await expect(initializeConnector()).rejects.toThrow('spawn failed for scanner')

        // The reaped processes report their exits late, after the connector has
        // already delivered the single startup-failure notification. They must not
        // also look like a crash that took the tree down by itself.
        for (const record of spawned) {
            record.child.emit('exit', { source: record.child.name, code: null })
        }
        await flush()

        expect(mocks.notifyBrokerStartupFailure).toHaveBeenCalledTimes(1)
        expect(mocks.notifyBrokerCrash).not.toHaveBeenCalled()
    })

    it('rejects a readiness waiter armed before the failed start', async () => {
        // A waiter armed while an earlier lifetime still had live processes and no
        // app:ready must not outlive this start: it rejects now rather than hanging
        // to its deadline.
        supervisor['isReady'] = true
        supervisor['brokerReady'] = false
        const staleWaiter = supervisor.waitUntilReady(200)
        supervisor['isReady'] = false

        await expect(initializeConnector()).rejects.toThrow('spawn failed for scanner')

        await expect(staleWaiter).rejects.toThrow(/failed to start/)
        expect(supervisor['readinessWaiters'].size).toBe(0)
        expect(supervisor.ready).toBe(false)
        expect(mocks.notifyBrokerStartupFailure).toHaveBeenCalledTimes(1)
        expect(mocks.notifyBrokerCrash).not.toHaveBeenCalled()
    })

    it('never touches the children of a newer start while a cleanup is in flight', async () => {
        holdTeardown = true
        await expect(initializeConnector()).rejects.toThrow('spawn failed for scanner')
        const firstAttempt = spawned.slice()

        // A new attempt starts while the first one's teardown is still parked on
        // the broker's shutdown grace.
        failSpawnName = null
        const starting = initializeConnector()
        const newBroker = supervisor['processes'].get('broker')
        const newWorker = supervisor['processes'].get('scanner')
        expect(newBroker).toBeDefined()
        expect(newBroker).not.toBe(firstAttempt[0]?.child)

        markBrokerReady()
        await starting
        expect(getConnectorStatus()).toBe('connected')

        // The older cleanup finished without reaching the newer attempt: its own
        // instances only, and it never cleared the newer map entries.
        holdTeardown = false
        releaseTeardowns()
        await flush()

        expect(firstAttempt.map(record => record.graces)).toEqual([[15_000], [undefined]])
        expect(spawned[2]?.graces).toEqual([])
        expect(spawned[3]?.graces).toEqual([])
        expect(supervisor['processes'].get('broker')).toBe(newBroker)
        expect(supervisor['processes'].get('scanner')).toBe(newWorker)
        expect(mocks.notifyBrokerStartupFailure).toHaveBeenCalledTimes(1)
        expect(mocks.notifyBrokerCrash).not.toHaveBeenCalled()

        spawned[2]?.child.emit('exit', { source: 'broker', code: null })
        await flush()
        expect(mocks.notifyBrokerCrash).toHaveBeenCalledTimes(1)
    })

    it('reaps the tree when the failure lands after every child spawned', async () => {
        failSpawnName = null
        vi.mocked(startNodeInfoPoller).mockImplementationOnce(() => {
            throw new Error('node info poller refused to start')
        })

        await expect(initializeConnector()).rejects.toThrow('node info poller refused to start')
        await flush()

        expect(spawned.map(record => record.graces)).toEqual([[15_000], [undefined]])
        expect(supervisor['processes'].size).toBe(0)
        expect(supervisor.ready).toBe(false)
        expect(mocks.notifyBrokerStartupFailure).toHaveBeenCalledTimes(1)
        expect(mocks.notifyBrokerCrash).not.toHaveBeenCalled()
    })

    it('reports nothing when the same start path succeeds', async () => {
        failSpawnName = null
        const starting = initializeConnector()
        markBrokerReady()
        await starting

        expect(getConnectorStatus()).toBe('connected')
        expect(didWeSpawnCli()).toBe(true)
        expect(spawned.map(record => record.graces)).toEqual([[], []])
        expect(supervisor.hasProcess('broker')).toBe(true)
        expect(supervisor.ready).toBe(true)
        expect(mocks.notifyBrokerStartupFailure).not.toHaveBeenCalled()
        expect(mocks.notifyBrokerCrash).not.toHaveBeenCalled()
    })
})
