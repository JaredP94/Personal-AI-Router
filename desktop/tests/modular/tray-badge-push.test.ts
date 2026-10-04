// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { startTrayBadge, type TrayBadgeSink } from '@/electron/tray-badge'
import { publishPush } from '@/electron/service-bridge/push-bus'
import { TRAY_ALERT_WINDOW_MS } from '@/shared/utils/tray-badge-state'
import type { Workload } from '@/shared/types/workloads'

/**
 * The tray icon is a non-window consumer of the same push stream the UI renders.
 * What matters is that it costs the platform nothing when nothing changed: a
 * backend restart emits one `workloads:remove` per catalog entry, and honouring
 * each as a fresh `setImage` is a repaint storm over an icon that looks identical
 * the whole way through.
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

/** BGRA ink of the first opaque pixel, as R/G/B. */
function inksOf(pixels: Uint8Array): number[] {
    for (let i = 0; i < pixels.length; i += 4) {
        if (pixels[i + 3] > 127) return [pixels[i + 2], pixels[i + 1], pixels[i]]
    }
    return []
}

/** A sink that records what the icon was asked to become, by pixel fingerprint. */
function recorder() {
    const icons: string[] = []
    const inks: number[][] = []
    const tips: string[] = []
    const sink: TrayBadgeSink = {
        setIcon: map => {
            let hash = 0
            for (let i = 3; i < map.pixels.length; i += 4) {
                hash = (hash * 31 + (map.pixels[i] > 127 ? 1 : 0)) | 0
            }
            const opaque = inksOf(map.pixels)
            inks.push(opaque)
            icons.push(`${hash}:${opaque.join(',')}`)
        },
        setToolTip: text => {
            tips.push(text)
        }
    }
    return { sink, icons, inks, tips }
}

let rec: ReturnType<typeof recorder>
let stop: (() => void) | null = null

beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(T0)
    rec = recorder()
    stop = null
})

afterEach(() => {
    stop?.()
    stop = null
    vi.useRealTimers()
})

describe('menu bar badge controller', () => {
    it('paints the idle icon before any job has been reported', () => {
        stop = startTrayBadge(rec.sink, [])

        expect(rec.icons).toHaveLength(1)
    })

    it('repaints once per change in what the icon shows', () => {
        stop = startTrayBadge(rec.sink, [])

        publishPush('workloads:upsert', workload({ id: '1', state: 'running' }))
        publishPush('workloads:upsert', workload({ id: '2', state: 'queued' }))
        publishPush('workloads:upsert', workload({ id: '2', state: 'running' }))
        publishPush('workloads:upsert', workload({ id: '1', state: 'running' }))

        expect(rec.icons).toHaveLength(3)
    })

    it('clears the icon when the last job leaves the catalog', () => {
        stop = startTrayBadge(rec.sink, [workload({ id: '1', state: 'running' })])

        publishPush('workloads:remove', { workloadId: '1', originatedFrom: 'node-a' })

        expect(rec.icons).toHaveLength(2)
        expect(rec.icons[0]).not.toBe(rec.icons[1])
    })

    it('reports counts the hover can read', () => {
        stop = startTrayBadge(rec.sink, [
            workload({ id: '1', state: 'running' }),
            workload({ id: '2', state: 'queued' }),
            workload({ id: '3', state: 'failed', completedAt: T0 - 1_000 })
        ])

        expect(rec.tips.at(-1)).toContain('2 active')
        expect(rec.tips.at(-1)).toContain('1 failed')
    })

    it('says nothing about jobs on the hover when it is idle', () => {
        stop = startTrayBadge(rec.sink, [])

        expect(rec.tips.at(-1)).not.toContain('active')
    })

    it('clears the alert when its window closes, not when the job leaves', () => {
        stop = startTrayBadge(rec.sink, [
            workload({ id: '1', state: 'failed', completedAt: T0 - 1_000 })
        ])
        const alerting = rec.icons.length

        // The job failed a second before start, so its window closes 29s in.
        vi.advanceTimersByTime(TRAY_ALERT_WINDOW_MS - 2_000)
        expect(rec.icons).toHaveLength(alerting)

        vi.advanceTimersByTime(4_000)
        expect(rec.icons).toHaveLength(alerting + 1)
    })

    it('tints the alert on a platform that can show colour', () => {
        stop = startTrayBadge(rec.sink, [], { alertInk: { r: 255, g: 0, b: 0 } })

        expect(rec.inks[0]).toEqual([255, 255, 255])

        publishPush('workloads:upsert', workload({ id: '1', state: 'failed', completedAt: T0 - 1 }))

        expect(rec.inks.at(-1)).toEqual([255, 0, 0])
    })

    it('stops repainting once unsubscribed', () => {
        stop = startTrayBadge(rec.sink, [])
        stop()
        stop = null
        const before = rec.icons.length

        publishPush('workloads:upsert', workload({ id: '9', state: 'running' }))

        expect(rec.icons).toHaveLength(before)
    })
})
