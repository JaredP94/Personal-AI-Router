// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * What the menu bar icon shows about job activity, derived from the workload
 * catalog. Pure — no Electron, no timers — so the rules that decide what ships
 * on screen are unit-testable and shared by the renderer and the tray.
 */
import type { Workload, WorkloadState } from '@/shared/types/workloads'

/**
 * A badge counts work that has not finished. `completed` and `failed` are
 * retained in the catalog (until a backend restart evicts them) and must not
 * hold a number on the menu bar after the work is done.
 */
const ACTIVE_STATES: readonly WorkloadState[] = ['initializing', 'queued', 'running']

/**
 * How long a failed job keeps the alert glyph lit. The backend never removes a
 * `failed` catalog entry on its own, so an unbounded alert would stay on screen
 * until the next restart and stop being read as news.
 */
export const TRAY_ALERT_WINDOW_MS = 30_000

/** Largest count drawn as digits; anything above this renders as `9+`. */
export const TRAY_BADGE_MAX_COUNT = 9

/** The two facts the tray icon can express, and the only two it redraws for. */
export interface TrayBadge {
    activeCount: number
    alert: boolean
}

/**
 * The catalog fields the badge reads. Nothing else about a job can change what
 * the icon looks like, so the derivation stays independent of the catalog's
 * growth (tokens, routing decisions, node attribution).
 */
type TrayBadgeInput = Pick<Workload, 'state' | 'createdAt' | 'startedAt' | 'completedAt'>

/** When a job last made a difference to the alert: it ended, ran, or arrived. */
function lastActivityAt(workload: TrayBadgeInput): number {
    return workload.completedAt ?? workload.startedAt ?? workload.createdAt
}

export function deriveTrayBadge(workloads: Iterable<TrayBadgeInput>, now: number): TrayBadge {
    let activeCount = 0
    let alert = false

    for (const workload of workloads) {
        if (ACTIVE_STATES.includes(workload.state)) {
            activeCount++
            continue
        }
        if (workload.state === 'failed' && now - lastActivityAt(workload) < TRAY_ALERT_WINDOW_MS) {
            alert = true
        }
    }

    return { activeCount, alert }
}

/**
 * Identity of the badge the icon would render. Two catalog states that draw the
 * same pixels share a signature, so a burst of upserts that nets no visible
 * change costs no native `setImage` call.
 */
export function trayBadgeSignature(badge: TrayBadge): string {
    const count =
        badge.activeCount > TRAY_BADGE_MAX_COUNT
            ? `${TRAY_BADGE_MAX_COUNT}+`
            : String(badge.activeCount)
    return `${count}:${badge.alert ? 'alert' : 'ok'}`
}
