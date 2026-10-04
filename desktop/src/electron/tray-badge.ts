// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Keeps the menu bar icon in step with job activity.
 *
 * The icon is a non-window consumer of the same push stream the tray popover
 * renders, so it subscribes to the in-process bus rather than going through a
 * renderer — the popover is closed almost all of the time, and an icon that only
 * updates while its window is visible would never show anything.
 *
 * Deliberately free of Electron: the platform hands it a sink, which keeps the
 * repaint rule (`only when the pixels change`) testable without a tray.
 */
import { subscribePush } from '@/electron/service-bridge/push-bus'
import { APP_DISPLAY_NAME } from '@/shared/constants/app'
import type { Workload } from '@/shared/types/workloads'
import {
    TRAY_ALERT_WINDOW_MS,
    deriveTrayBadge,
    trayBadgeSignature,
    type TrayBadge
} from '@/shared/utils/tray-badge-state'
import {
    renderTrayBadgeMap,
    type TrayBadgeInk,
    type TrayBadgeMap
} from '@/shared/utils/tray-badge-map'
import { workloadKey } from '@/shared/utils/workloads'

export interface TrayBadgeSink {
    setIcon: (map: TrayBadgeMap) => void
    setToolTip: (text: string) => void
}

interface TrayBadgeOptions {
    /**
     * Ink for the alert glyph. macOS paints a template image in the menu bar's
     * own colour, so a platform that renders the icon verbatim (Windows, Linux)
     * passes red here; macOS passes nothing and the `!` shape carries the alert.
     */
    alertInk?: TrayBadgeInk
}

/** When a failure stops being news. */
function alertExpiresAt(workload: Workload): number {
    return (workload.completedAt ?? workload.startedAt ?? workload.createdAt) + TRAY_ALERT_WINDOW_MS
}

function tooltipFor(badge: TrayBadge, failedCount: number): string {
    if (badge.activeCount <= 0 && failedCount <= 0) return APP_DISPLAY_NAME
    const parts: string[] = []
    if (badge.activeCount > 0) parts.push(`${badge.activeCount} active`)
    if (failedCount > 0) parts.push(`${failedCount} failed`)
    return `${APP_DISPLAY_NAME} — ${parts.join(' · ')}`
}

/**
 * Subscribe to job activity and repaint the icon only when it would look
 * different. Returns the teardown; calling it twice is harmless.
 */
export function startTrayBadge(
    sink: TrayBadgeSink,
    initial: Iterable<Workload>,
    options?: TrayBadgeOptions
): () => void {
    const catalog = new Map<string, Workload>()
    for (const workload of initial) {
        catalog.set(workloadKey(workload.originatedFrom, workload.id), workload)
    }

    let signature = ''
    let expiryTimer: ReturnType<typeof setTimeout> | undefined

    const clearExpiry = (): void => {
        if (expiryTimer === undefined) return
        clearTimeout(expiryTimer)
        expiryTimer = undefined
    }

    const refresh = (now: number): void => {
        const workloads = [...catalog.values()]
        const badge = deriveTrayBadge(workloads, now)
        const next = trayBadgeSignature(badge)
        if (next === signature) return
        signature = next

        const ink = badge.alert ? options?.alertInk : undefined
        sink.setIcon(renderTrayBadgeMap(badge, ink ? { ink } : undefined))
        sink.setToolTip(tooltipFor(badge, workloads.filter(w => w.state === 'failed').length))

        clearExpiry()
        if (!badge.alert) return
        // Re-report when the oldest live alert expires; a later failure that is
        // still inside the window keeps the alert lit and picks up the next timer.
        const upcoming = workloads
            .filter(w => w.state === 'failed')
            .map(alertExpiresAt)
            .filter(at => at > now)
            .sort((a, b) => a - b)[0]
        if (upcoming !== undefined) {
            expiryTimer = setTimeout(() => void refresh(Date.now()), upcoming - now)
        }
    }

    const unsubscribe = subscribePush(event => {
        if (event.channel === 'workloads:upsert') {
            catalog.set(workloadKey(event.payload.originatedFrom, event.payload.id), event.payload)
        } else if (event.channel === 'workloads:remove') {
            catalog.delete(workloadKey(event.payload.originatedFrom, event.payload.workloadId))
        } else {
            return
        }
        refresh(Date.now())
    })

    refresh(Date.now())

    return () => {
        unsubscribe()
        clearExpiry()
    }
}
