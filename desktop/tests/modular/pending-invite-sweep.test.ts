// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { JsonValue } from '@/shared/types/json'
import type { JsonRpcNotification } from '@/electron/service-bridge/json-rpc-subprocess'

/**
 * Relay protocol v2 retires the bridge's 1 500 ms pending-invite sweep. Every
 * terminal inbound invite state now arrives as a cluster-manager push and is
 * reconciled in handleClusterManagerNotification, so nothing in the bridge may
 * poll `cluster:invite-status` on a timer.
 *
 * The two timer assertions are behavioural and run through public paths under a
 * fake clock — the real ModularSupervisor, the real ModularBridgeState
 * pending-invite set, and the real broker-hydration pass. Neither reads source
 * text to decide whether the sweep is present, so a no-op'd or renamed sweep
 * fails here exactly as a live one does: a live one shows up as
 * `cluster:invite-status` traffic on the wire.
 */

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

import { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'
import { getModularBridgeState } from '@/electron/service-bridge/modular-state'

/** The period the retired sweep ran at. */
const SWEEP_PERIOD_MS = 1_500

/** The wire shape the cluster-manager pushes for one invite. */
function invite(overrides: { [key: string]: JsonValue } = {}): { [key: string]: JsonValue } {
    return {
        inviteId: 'invite-sweep-1',
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

describe('pending-invite sweep removal', () => {
    /** Every broker method the bridge called, in order. */
    let calls: string[]

    function notify(method: string, params?: JsonValue): void {
        const notification: JsonRpcNotification = { source: 'broker', method, params }
        getModularSupervisor()['handleNotification'](notification)
    }

    /** Run the real broker-hydration pass with no subprocesses behind it. */
    async function hydrate(): Promise<void> {
        getModularSupervisor()['brokerHydrationDone'] = false
        getModularSupervisor()['brokerReady'] = true
        await getModularSupervisor()['onBrokerReady']()
    }

    /** How often the bridge asked the cluster-manager for an invite status. */
    function statusPolls(): number {
        return calls.filter(method => method === 'cluster:invite-status').length
    }

    beforeEach(() => {
        calls = []
        vi.useFakeTimers()

        getModularSupervisor()['callProcess'] = (_name, method) => {
            calls.push(method)
            // Answer the retired sweep's status probe with a still-pending
            // invite, so a live sweep keeps re-probing on every tick instead of
            // pruning once and going quiet.
            if (method === 'cluster:invite-status') return Promise.resolve(invite())
            return Promise.resolve({})
        }
    })

    afterEach(() => {
        for (const entry of getModularBridgeState().getPendingInvites()) {
            getModularBridgeState().prunePendingInvite(entry.inviteId)
        }
        vi.useRealTimers()
    })

    it('never asks for an invite status on a 1 500 ms cadence', async () => {
        const state = getModularBridgeState()
        notify('cluster:invite-received', invite())
        expect(state.getPendingInvites()).toHaveLength(1)

        await hydrate()
        calls.length = 0

        // Forty whole sweep periods. A live sweep lands on every one of them.
        for (let tick = 0; tick < 40; tick++) {
            await vi.advanceTimersByTimeAsync(SWEEP_PERIOD_MS)
        }

        expect(statusPolls()).toBe(0)
    })

    it('leaves a pending invite alone no matter how long the clock runs', async () => {
        const state = getModularBridgeState()
        notify('cluster:invite-received', invite())
        expect(state.getPendingInvites()).toHaveLength(1)

        await hydrate()
        calls.length = 0

        // One long jump rather than steady ticks: a missed push must not be
        // papered over by a later timer, which is what the sweep used to do.
        await vi.advanceTimersByTimeAsync(60_000)

        expect(statusPolls()).toBe(0)
        expect(state.getPendingInvites()).toHaveLength(1)
    })

    it('still clears a pending invite when it expires', async () => {
        const state = getModularBridgeState()
        notify('cluster:invite-received', invite())
        expect(state.getPendingInvites()).toHaveLength(1)

        notify('cluster:invite-expired', invite({ state: 'expired', pin: null }))

        expect(state.getPendingInvites()).toEqual([])
    })

    it('still clears a pending invite when the inviter cancels it', async () => {
        const state = getModularBridgeState()
        notify('cluster:invite-received', invite())
        expect(state.getPendingInvites()).toHaveLength(1)

        notify('cluster:invite-canceled', invite({ state: 'canceled', pin: null }))

        expect(state.getPendingInvites()).toEqual([])
    })

    it('still clears a pending invite when the pairing completes', async () => {
        const state = getModularBridgeState()
        notify('cluster:invite-received', invite())
        expect(state.getPendingInvites()).toHaveLength(1)

        // Pairing turns the inviter into a confirmed member, so the inbound
        // invite is obsolete. This is the push that replaces the sweep for
        // `paired` — there is no cluster:invite-paired notification.
        notify('nodes:changed', {
            nodes: [
                {
                    id: 'sender-uuid',
                    nodeUuid: 'sender-uuid',
                    name: 'Sender',
                    state: 'member',
                    engineCount: 0,
                    addresses: []
                }
            ]
        })

        expect(state.getPendingInvites()).toEqual([])
    })
})
