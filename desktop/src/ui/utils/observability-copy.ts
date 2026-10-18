// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { TelemetryStatus, TelemetryStorage } from '@/shared/types/telemetry'
import { formatBytes, formatRelativeTime } from '@/ui/utils/formatters'

export const observabilityCopy = {
    consent: {
        title: 'Capture prompts and responses?',
        body: "PAIR will attach the text of prompts and model responses (up to 16 KB each) to traces sent to Phoenix on this machine. Captured text may contain personal or confidential information and stays in Phoenix's storage until you clear it. Turning capture off stops new capture but can't remove text already stored.",
        confirm: 'Capture text',
        cancel: 'Cancel'
    },
    clear: {
        title: 'Clear stored traces?',
        body: 'This permanently deletes the traces stored by Phoenix on this machine. Phoenix stops while the data is deleted.',
        confirm: 'Delete traces',
        cancel: 'Cancel'
    }
}

export function observabilityWarnings(status: TelemetryStatus | null): string[] {
    if (!status) return []
    const warnings: string[] = []
    if (status.enabled && !status.collectorReachable && status.containerState !== 'running') {
        warnings.push(
            "Tracing is on, but Phoenix isn't running, so this node's traces are being dropped."
        )
    }
    if (status.bindingsLoopbackOnly === false) {
        warnings.push(
            'Phoenix is reachable from other machines on your network. Restart it to apply the local-only configuration.'
        )
    }
    if (status.managedConfig === 'outdated') {
        warnings.push(
            'Phoenix is running an older PAIR configuration. Restart it to apply the current one.'
        )
    }
    for (const worker of status.workers) {
        if (worker.state === 'failed') {
            warnings.push(
                `Tracing settings didn't reach ${worker.name}. Restart PAIR's services from this page if it persists.`
            )
        }
    }
    return warnings
}

export function isCollectorPortValid(portStr: string): boolean {
    const trimmed = portStr.trim()
    if (!/^\d+$/.test(trimmed)) return false
    const n = Number(trimmed)
    return Number.isInteger(n) && n >= 1 && n <= 65535
}

export function formatTelemetryStorage(storage: TelemetryStorage | null | undefined): string {
    if (!storage || storage.bytes === null) {
        return 'Storage size unavailable'
    }
    const formatted = formatBytes(storage.bytes)
    if (storage.measuredAt) {
        const time = new Date(storage.measuredAt).getTime()
        if (!isNaN(time)) {
            return `${formatted} (${formatRelativeTime(time)})`
        }
    }
    return formatted
}

export function scheduleTelemetryPolling(
    getStatus: () => Promise<TelemetryStatus>,
    onStatus: (status: TelemetryStatus) => void,
    onError?: (err: unknown) => void,
    interval: number = 4000
): () => void {
    let unmounted = false
    let loaded = false

    const poll = async () => {
        if (unmounted) return
        try {
            const nextStatus = await getStatus()
            if (!unmounted) {
                loaded = true
                onStatus(nextStatus)
            }
        } catch (err) {
            if (!unmounted && !loaded && onError) {
                onError(err)
            }
        }
    }

    void poll()
    const intervalId = setInterval(() => {
        void poll()
    }, interval)

    return () => {
        unmounted = true
        clearInterval(intervalId)
    }
}
