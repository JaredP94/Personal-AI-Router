/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

export type TelemetryPollCadence = 'starting' | 'active'

interface TelemetryPoller {
    setCadence: (cadence: TelemetryPollCadence) => void
    stop: () => void
}

export function createTelemetryPoller(
    fetchStatus: () => Promise<void>,
    intervalForCadence: (cadence: TelemetryPollCadence) => number
): TelemetryPoller {
    let currentCadence: TelemetryPollCadence | null = null
    let stopped = false
    let inFlight = false
    let intervalId: ReturnType<typeof setInterval> | null = null

    const triggerFetch = async () => {
        if (stopped || inFlight) {
            return
        }
        inFlight = true
        try {
            await fetchStatus()
        } finally {
            inFlight = false
        }
    }

    const clearTimer = () => {
        if (intervalId !== null) {
            clearInterval(intervalId)
            intervalId = null
        }
    }

    return {
        setCadence: (cadence: TelemetryPollCadence) => {
            if (!stopped && currentCadence === cadence) {
                return
            }
            stopped = false
            currentCadence = cadence
            clearTimer()

            void triggerFetch()
            const interval = intervalForCadence(cadence)
            intervalId = setInterval(() => {
                void triggerFetch()
            }, interval)
        },
        stop: () => {
            stopped = true
            currentCadence = null
            clearTimer()
        }
    }
}
