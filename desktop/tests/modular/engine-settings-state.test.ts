// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type {
    EngineSettingsSnapshot,
    EngineSettingsTarget,
    EngineSettingsReceipt
} from '@/shared/types/engine-settings'
import {
    engineSettingsKey,
    useEngineSettingsStore,
    watchEngineSettings
} from '@/ui/stores/engine-settings.store'

const target: EngineSettingsTarget = { nodeId: 'peer', engine: 'ollama' }
const baseline: EngineSettingsSnapshot = {
    ...target,
    revision: 1,
    appliedRevision: 1,
    sequence: 1,
    epoch: 'one',
    settings: { serverPort: 11435, proxyPort: 11434, launchText: '' },
    effectiveServerPort: 11435,
    effectiveProxyPort: 11434,
    running: true,
    adopted: false,
    editable: true,
    reason: '',
    format: 'pair-arguments-v1',
    phase: 'idle',
    error: '',
    requestId: ''
}
const store = useEngineSettingsStore
const key = engineSettingsKey(target)
const getSettings = vi.fn<() => Promise<EngineSettingsSnapshot>>()
const applySettings = vi.fn<() => Promise<EngineSettingsReceipt>>()

describe('engine settings authority and recovery', () => {
    beforeEach(() => {
        store.setState({ entries: {} })
        getSettings.mockReset().mockResolvedValue(baseline)
        applySettings.mockReset().mockResolvedValue({ revision: 2, phase: 'applying' })
        vi.stubGlobal('window', { pairApi: { engines: { getSettings, applySettings } } })
    })
    afterEach(() => vi.unstubAllGlobals())

    it('ignores out-of-order snapshots and accepts an identical reconnect baseline', () => {
        store.getState().receive({ ...baseline, sequence: 3, revision: 2 })
        store.getState().receive(baseline)
        expect(store.getState().entries[key].snapshot?.revision).toBe(2)
        store.getState().disconnect('peer')
        expect(store.getState().entries[key].unavailable).toBeTruthy()
        store.getState().receive({ ...baseline, sequence: 3, revision: 2 })
        expect(store.getState().entries[key].unavailable).toBeUndefined()
    })

    it('a new-epoch push wins over a delayed read', async () => {
        let finish = (_snapshot: EngineSettingsSnapshot): void => {}
        getSettings.mockImplementationOnce(
            () =>
                new Promise(resolve => {
                    finish = resolve
                })
        )
        const loading = store.getState().reload(target)
        store.getState().receive({ ...baseline, epoch: 'two', revision: 9 })
        finish(baseline)
        await loading
        expect(store.getState().entries[key].snapshot?.epoch).toBe('two')
    })

    it('disconnect during a read cannot be undone by its delayed reply', async () => {
        let finish = (_snapshot: EngineSettingsSnapshot): void => {}
        getSettings.mockImplementationOnce(
            () =>
                new Promise(resolve => {
                    finish = resolve
                })
        )
        const loading = store.getState().reload(target)
        store.getState().disconnect('peer')
        finish(baseline)
        await loading
        expect(store.getState().entries[key].snapshot).toBeUndefined()
        expect(store.getState().entries[key].unavailable).toBeTruthy()
    })

    it('keeps progress covered after an applying receipt until its own snapshot arrives', async () => {
        store.getState().receive(baseline)
        await store.getState().apply({
            ...target,
            expectedRevision: 1,
            requestId: 'request',
            settings: baseline.settings
        })
        expect(store.getState().entries[key].snapshot?.revision).toBe(1)
        expect(store.getState().entries[key].pending).toBe('request')
        store.getState().receive({ ...baseline, sequence: 2, requestId: 'other' })
        expect(store.getState().entries[key].pending).toBe('request')
        store.getState().receive({
            ...baseline,
            sequence: 3,
            revision: 2,
            requestId: 'request',
            phase: 'applying'
        })
        expect(store.getState().entries[key].pending).toBeUndefined()
        expect(store.getState().entries[key].snapshot?.phase).toBe('applying')
    })

    it('recovers a terminal snapshot when its push was missed', async () => {
        store.getState().receive(baseline)
        applySettings.mockResolvedValue({ revision: 2, phase: 'succeeded' })
        getSettings.mockResolvedValue({
            ...baseline,
            sequence: 2,
            revision: 2,
            requestId: 'request',
            phase: 'succeeded'
        })
        await store.getState().apply({
            ...target,
            expectedRevision: 1,
            requestId: 'request',
            settings: baseline.settings
        })
        expect(store.getState().entries[key].snapshot?.phase).toBe('succeeded')
        expect(store.getState().entries[key].pending).toBeUndefined()
    })

    it('shares push subscriptions, retaining snapshots when editors close', () => {
        const stop = vi.fn()
        vi.stubGlobal('window', {
            pairApi: {
                engines: {
                    onSettingsChanged: (receive: (snapshot: EngineSettingsSnapshot) => void) => {
                        receive(baseline)
                        return stop
                    },
                    onSettingsDisconnected: () => stop
                }
            }
        })
        const a = watchEngineSettings(),
            b = watchEngineSettings()
        a()
        expect(stop).not.toHaveBeenCalled()
        b()
        expect(stop).toHaveBeenCalledTimes(2)
        expect(store.getState().entries[key].snapshot?.revision).toBe(1)
        expect(store.getState().entries[key].unavailable).toBeUndefined()
    })
})
