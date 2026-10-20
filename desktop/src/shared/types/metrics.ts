// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export interface GpuMetricValue {
    id: string // GPU ID from topology
    value: number | null // percentage, null when not measured
}

interface GpuMemoryMetricValue extends GpuMetricValue {
    sharedUsed: number | null // Windows shared-system-memory usage, in bytes
}

export interface NodeItemMetricsEntry {
    timestamp: number // milliseconds since epoch
    cpuUtilization: number // percentage
    memoryUsage: number // percentage
    gpuUtilization: GpuMetricValue[] // percentage per GPU
    gpuVramUsage: GpuMemoryMetricValue[] // percentage per GPU, plus shared usage in bytes
}

export interface NodeItemMetrics {
    id: string
    current: NodeItemMetricsEntry
    historical: NodeItemMetricsEntry[]
}
