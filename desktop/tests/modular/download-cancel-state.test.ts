// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, expect, it, vi } from 'vitest'
vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))
import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import { subscribePush } from '@/electron/service-bridge/push-bus'
import type { EngineProgress } from '@/shared/types/engines'

afterEach(() => vi.useRealTimers())

it('keeps Canceling through late progress and clears only on confirmed stop', () => {
    vi.useFakeTimers()
    const state = getModularBridgeState()
    state.setSelfId('cancel-local')
    const updates: EngineProgress[] = []
    const off = subscribePush(e => {
        if (e.channel === 'engines:progress-changed') updates.push(e.payload)
    })
    const opId = state.beginModelPull('ollama', 'cancel-test')
    state.applyLocalEngineProgress({
        engine: 'ollama',
        model: 'cancel-test',
        opId,
        stage: 'downloading',
        canCancel: true,
        percent: 20
    })
    expect(state.markModelPullCanceling('cancel-local', 'ollama', 'cancel-test', opId)).toBe(true)
    state.applyLocalEngineProgress({
        engine: 'ollama',
        model: 'cancel-test',
        opId,
        stage: 'downloading',
        canCancel: true,
        percent: 30
    })
    expect(updates.at(-1)?.status).toBe('canceling')
    state.finishModelPull('ollama', 'cancel-test', opId)
    expect(state.isModelPullActive('ollama', 'cancel-test')).toBe(true)
    state.applyLocalEngineProgress({
        engine: 'ollama',
        model: 'cancel-test',
        opId,
        stage: 'cancelled'
    })
    expect(updates.at(-1)?.status).toBe('cancelled')
    expect(state.isModelPullActive('ollama', 'cancel-test')).toBe(false)
    vi.advanceTimersByTime(8_000)
    off()
})

it('fences old progress, stopped updates, RPC completion and notice timers by operation ID', () => {
    vi.useFakeTimers()
    const state = getModularBridgeState()
    state.setSelfId('cancel-local')
    const updates: EngineProgress[] = []
    const off = subscribePush(e => {
        if (e.channel === 'engines:progress-changed') updates.push(e.payload)
    })
    const old = state.beginRemoteModelPull('peer-record', 'lm-studio', 'owner/model')
    state.applyRemoteEngineProgress({
        node: 'peer-record',
        engine: 'lmstudio',
        model: 'owner/model',
        opId: old,
        op: 'pull',
        stage: 'cancelled'
    })
    const current = state.beginRemoteModelPull('peer-record', 'lm-studio', 'owner/model')
    for (const stage of ['downloading', 'cancelled', 'done'])
        state.applyRemoteEngineProgress({
            node: 'peer-record',
            engine: 'lmstudio',
            model: 'owner/model',
            opId: old,
            op: 'pull',
            stage,
            percent: 90
        })
    state.finishRemoteModelPull('peer-record', 'lm-studio', 'owner/model', old)
    vi.advanceTimersByTime(8_000)
    expect(updates.at(-1)?.opId).toBe(current)
    expect(updates.at(-1)?.status).toBe('pulling')
    expect(state.isRemoteModelPullActive('peer-record', 'lm-studio', 'owner/model')).toBe(true)
    off()
})
