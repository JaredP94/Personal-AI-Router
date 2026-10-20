// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { getModularBridgeState } from '@/electron/service-bridge/modular-state'

type BridgeState = ReturnType<typeof getModularBridgeState>

/**
 * One Electron window as the poller reads it: Electron's own `BrowserWindow` is
 * three predicates for this decision, and a tray-only session is an empty list.
 */
interface FakeWindow {
    isDestroyed: () => boolean
    isVisible: () => boolean
    isMinimized: () => boolean
}

const mocks = vi.hoisted(() => ({
    state: {
        getNodeInfoPollTargets: vi.fn<BridgeState['getNodeInfoPollTargets']>(),
        mergeNodeInfoResponse: vi.fn<BridgeState['mergeNodeInfoResponse']>()
    },
    windows: [] as FakeWindow[],
    windowQueryFailed: false
}))

vi.mock('electron', () => ({
    BrowserWindow: {
        getAllWindows: () => {
            if (mocks.windowQueryFailed) throw new Error('BrowserWindow unavailable')
            return mocks.windows
        }
    }
}))

vi.mock('@/electron/service-bridge/modular-state', () => ({
    getModularBridgeState: () => mocks.state
}))

vi.mock('@/shared/utils/log', () => ({
    createStructuredLogger: () => ({
        info: () => {},
        warn: () => {},
        error: () => {},
        verbose: () => {}
    })
}))

import { startNodeInfoPoller, stopNodeInfoPoller } from '@/electron/service-bridge/node-info-poller'

const fetchMock = vi.fn<typeof fetch>()

function shown(): FakeWindow {
    return { isDestroyed: () => false, isVisible: () => true, isMinimized: () => false }
}

function hidden(): FakeWindow {
    return { isDestroyed: () => false, isVisible: () => false, isMinimized: () => false }
}

function minimized(): FakeWindow {
    return { isDestroyed: () => false, isVisible: () => true, isMinimized: () => true }
}

/** The `X-NVPAIR-Poll` header of one recorded fetch. */
function pollHeaderOf(init: RequestInit | undefined): string {
    const headers = init?.headers
    if (!headers || Array.isArray(headers) || headers instanceof Headers) return ''
    return headers['X-NVPAIR-Poll'] ?? ''
}

/**
 * Start the poller against the current window list and report every poll intent
 * it sent. A visible window also means the faster two-second cadence, so the test
 * reads one tick rather than waiting for an interval.
 */
async function pollIntents(): Promise<string[]> {
    startNodeInfoPoller()
    await vi.advanceTimersByTimeAsync(0)
    const intents = fetchMock.mock.calls.map(([, init]) => pollHeaderOf(init))
    stopNodeInfoPoller()
    fetchMock.mockClear()
    return intents
}

// What the desktop tells node-info is what node-info's GPU sampling spends: a
// `foreground` poll renews the two-second interactive lease, so sending it with
// no window in front of the user keeps the hardware collector at full rate for a
// UI nobody is looking at. A tray-only session has no windows at all, which is
// the case that was being reported as "visible".
describe('node info poll intent', () => {
    beforeEach(() => {
        vi.useFakeTimers()
        vi.stubGlobal('fetch', fetchMock)
        fetchMock.mockReset()
        fetchMock.mockImplementation(() =>
            Promise.resolve(new Response(JSON.stringify({ hostUuid: 'uuid-a' }), { status: 200 }))
        )
        mocks.state.getNodeInfoPollTargets.mockReturnValue([
            { id: 'uuid-a', hosts: ['127.0.0.1'], port: 14318 }
        ])
        mocks.windows = []
        mocks.windowQueryFailed = false
    })

    afterEach(() => {
        stopNodeInfoPoller()
        vi.unstubAllGlobals()
        vi.useRealTimers()
    })

    it('reads in the background with no window at all, as in a tray-only session', async () => {
        expect(await pollIntents()).toEqual(['background'])
    })

    it('reads in the background while the overview window is hidden', async () => {
        mocks.windows = [hidden()]
        expect(await pollIntents()).toEqual(['background'])
    })

    it('reads in the foreground while the overview window is visible', async () => {
        mocks.windows = [shown()]
        expect(await pollIntents()).toEqual(['foreground'])
    })

    it('reads in the foreground while the tray popup is shown', async () => {
        // The tray popup is a real BrowserWindow; hidden it is only a renderer
        // kept warm, so the shown window is the one that counts.
        mocks.windows = [hidden(), shown()]
        expect(await pollIntents()).toEqual(['foreground'])
    })

    it('reads in the background while every window is minimized', async () => {
        mocks.windows = [minimized()]
        expect(await pollIntents()).toEqual(['background'])
    })

    it('reads in the background when the window list cannot be read', async () => {
        // A window query that throws says nothing about the user watching the
        // UI, and a wrong `foreground` costs the hardware collector its backoff.
        mocks.windowQueryFailed = true
        expect(await pollIntents()).toEqual(['background'])
    })

    it('reads in the background for a destroyed window still listed', async () => {
        mocks.windows = [
            { isDestroyed: () => true, isVisible: () => true, isMinimized: () => false }
        ]
        expect(await pollIntents()).toEqual(['background'])
    })
})
