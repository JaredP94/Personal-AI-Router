// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { GpuInfo as HardwareGpuInfo } from '@/shared/types/hardware'

export type GpuInfo = HardwareGpuInfo & {
    sharedUsed: number | null
    vramTotalFormatted: string
    vramUsageFormatted: string
    vramUsage: number | null // percentage
    usage: number | null // percentage
    usageColor: string
    vramColor: string
}

export type CpuFallbackInfo = {
    model: string
    usage: number // percentage 0-100
    usageColor: string
    memoryUsage: number // percentage 0-100
    memoryUsageFormatted: string
    memoryTotalFormatted: string
    memoryColor: string
}
