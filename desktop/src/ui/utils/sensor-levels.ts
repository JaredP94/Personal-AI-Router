// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { MemoryPressure, ThermalPressure } from '@/shared/types/metrics'

export type SensorTone = 'default' | 'amber' | 'red'

export function thermalTone(level: ThermalPressure | null): SensorTone {
    switch (level) {
        case 'moderate':
        case 'heavy':
            return 'amber'
        case 'trapping':
        case 'sleeping':
            return 'red'
        default:
            return 'default'
    }
}

export function memoryTone(level: MemoryPressure | null): SensorTone {
    switch (level) {
        case 'warning':
            return 'amber'
        case 'critical':
            return 'red'
        default:
            return 'default'
    }
}

// Reuse the feedback tokens used by engine freshness and error text.
export const sensorToneColors: Record<SensorTone, string | undefined> = {
    default: undefined,
    amber: 'var(--text-color-feedback-warning)',
    red: 'var(--text-color-feedback-danger)'
}
