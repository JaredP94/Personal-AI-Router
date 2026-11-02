// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Text } from '@nvidia/foundations-react-core'
import type { NodeSensors } from '@/shared/types/metrics'
import { memoryTone, sensorToneColors, thermalTone } from '@/ui/utils/sensor-levels'

export default function NodeSensorBadges({ sensors }: { sensors: NodeSensors | null }) {
    const thermal = thermalTone(sensors?.thermalPressure ?? null)
    const memory = memoryTone(sensors?.memoryPressure ?? null)
    if (thermal === 'default' && memory === 'default') return null

    return (
        <>
            {thermal !== 'default' && (
                <Text
                    kind="body/regular/sm"
                    className="rounded-full border px-2 py-0.5"
                    style={{ color: sensorToneColors[thermal] }}
                >
                    Throttling
                </Text>
            )}
            {memory !== 'default' && (
                <Text
                    kind="body/regular/sm"
                    className="rounded-full border px-2 py-0.5"
                    style={{ color: sensorToneColors[memory] }}
                >
                    Memory pressure
                </Text>
            )}
        </>
    )
}
