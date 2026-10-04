// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import type { Workload } from '@/shared/types/workloads'
import { deriveTrayBadge, trayBadgeSignature } from '@/shared/utils/tray-badge-state'

/**
 * The menu bar badge is a pixel-level readout of job activity, so every rule
 * about what counts has to be pinned down in code: an icon that silently counts
 * a `completed` job keeps a number on the screen after the work is done, and an
 * icon that never clears a `failed` one shouts forever (the backend retains
 * failed catalog entries until a restart).
 */

const T0 = 1_700_000_000_000

function workload(overrides: Partial<Workload> = {}): Workload {
    return {
        id: '1',
        model: 'llama3:8b',
        engine: 'ollama',
        state: 'running',
        originatedFrom: 'node-a',
        scheduledOn: 'node-a',
        createdAt: T0,
        startedAt: T0,
        completedAt: null,
        error: null,
        requesterId: null,
        ...overrides
    }
}

describe('menu bar badge state', () => {
    it('counts only jobs that have not ended', () => {
        const badge = deriveTrayBadge(
            [
                workload({ id: '1', state: 'running' }),
                workload({ id: '2', state: 'queued' }),
                workload({ id: '3', state: 'initializing' }),
                workload({ id: '4', state: 'completed' }),
                workload({ id: '5', state: 'failed' })
            ],
            T0
        )

        expect(badge.activeCount).toBe(3)
    })

    it('reports no activity for an empty catalog', () => {
        expect(deriveTrayBadge([], T0).activeCount).toBe(0)
    })

    it('flags an alert for a job that failed inside the alert window', () => {
        const badge = deriveTrayBadge(
            [workload({ state: 'failed', completedAt: T0 - 1000, error: 'engine died' })],
            T0
        )

        expect(badge.alert).toBe(true)
    })

    it('clears the alert once the failed job ages past the alert window', () => {
        const badge = deriveTrayBadge(
            [workload({ state: 'failed', completedAt: T0 - 31_000, error: 'engine died' })],
            T0
        )

        expect(badge.alert).toBe(false)
    })

    it('falls back to startedAt then createdAt when a failed job has no completedAt', () => {
        const recentStart = deriveTrayBadge(
            [workload({ state: 'failed', startedAt: T0 - 5_000, completedAt: null })],
            T0
        )
        const oldStart = deriveTrayBadge(
            [
                workload({
                    state: 'failed',
                    startedAt: null,
                    createdAt: T0 - 5_000,
                    completedAt: null
                })
            ],
            T0
        )
        const oldStartOnly = deriveTrayBadge(
            [workload({ state: 'failed', startedAt: T0 - 60_000, createdAt: T0 - 60_000 })],
            T0
        )

        expect(recentStart.alert).toBe(true)
        expect(oldStart.alert).toBe(true)
        expect(oldStartOnly.alert).toBe(false)
    })

    it('keeps the count visible while the alert shows a different glyph', () => {
        const badge = deriveTrayBadge(
            [
                workload({ id: '1', state: 'running' }),
                workload({ id: '2', state: 'failed', completedAt: T0 - 1_000 })
            ],
            T0
        )

        expect(badge).toEqual({ activeCount: 1, alert: true })
    })

    it('caps the rendered count at nine plus', () => {
        const many = Array.from({ length: 12 }, (_, i) => workload({ id: `${i}` }))

        expect(deriveTrayBadge(many, T0).activeCount).toBe(12)
        expect(trayBadgeSignature(deriveTrayBadge(many, T0))).toBe('9+:ok')
        expect(trayBadgeSignature({ activeCount: 0, alert: true })).toBe('0:alert')
    })

    it('changes signature only when the rendered badge would change', () => {
        const twoRunning = trayBadgeSignature(
            deriveTrayBadge(
                [workload({ id: '1', state: 'running' }), workload({ id: '2', state: 'running' })],
                T0
            )
        )
        const twoRunningOtherIds = trayBadgeSignature(
            deriveTrayBadge(
                [
                    workload({ id: '9', state: 'queued' }),
                    workload({ id: '10', state: 'initializing' })
                ],
                T0
            )
        )
        const oneRunning = trayBadgeSignature(deriveTrayBadge([workload({ id: '1' })], T0))

        expect(twoRunning).toBe(twoRunningOtherIds)
        expect(oneRunning).not.toBe(twoRunning)
    })
})
