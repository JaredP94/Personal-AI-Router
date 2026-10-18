// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { NodeItem } from '@/shared/types/nodes'
import type { RadialMetric } from '@/ui/components/GpuRadialChartSvg'
import type { NodeMetricsHistory } from '@/ui/stores/metrics.store'
import { CHART_COLORS } from '@/ui/constants/colors'
import { getGpuColor, getVramColor } from '@/ui/utils/colors'
import { hasInferenceReadyGpu, isGpuInferenceReady } from '@/ui/utils/gpu-inference'

function getLatest(data: { timestamp: number; value: number | null }[]): number | null {
    return data[data.length - 1]?.value ?? null
}

export function buildRadialChartMetrics(
    topology: NodeItem['topology'],
    nodeMetrics: NodeMetricsHistory | undefined,
    // Show only the GPUs the backend marks inference-ready (or all GPUs when it
    // reports no readiness list); see gpu-inference.ts. A node with no
    // inference-ready GPU falls through to the CPU + RAM rings below.
    hasGpu = hasInferenceReadyGpu(topology)
): RadialMetric[] {
    if (!hasGpu) {
        return [
            {
                value: nodeMetrics ? (getLatest(nodeMetrics.cpuUtilization) ?? 0) : 0,
                color: CHART_COLORS.CPU,
                shown: true
            },
            {
                value: nodeMetrics ? (getLatest(nodeMetrics.memoryUsage) ?? 0) : 0,
                color: CHART_COLORS.MEMORY,
                shown: true
            }
        ]
    }

    // Keep the original GPU index (metrics arrays and colors are keyed
    // positionally to topology.gpus) but omit the rings for any GPU the backend
    // did not mark inference-ready (when it sends a readiness list).
    return topology.gpus.flatMap((gpu, gpuIdx) => {
        if (!isGpuInferenceReady(gpu, topology.inferenceHardwareIds)) return []
        const gpuUtil = nodeMetrics?.gpuUtilization[gpuIdx]
        const gpuVram = nodeMetrics?.gpuVramUsage[gpuIdx]
        const utilization = gpuUtil ? getLatest(gpuUtil.data) : null
        const memoryUsage = gpuVram ? getLatest(gpuVram.data) : null
        const metrics: RadialMetric[] = []
        if (utilization !== null) {
            metrics.push({ value: utilization, color: getGpuColor(gpuIdx), shown: true })
        }
        if (memoryUsage !== null) {
            metrics.push({ value: memoryUsage, color: getVramColor(gpuIdx), shown: true })
        }
        return metrics
    })
}
