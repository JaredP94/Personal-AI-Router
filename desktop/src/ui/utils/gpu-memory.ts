// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { GpuInfo, GpuMemoryKind } from '@/shared/types/hardware'
import { formatBytes } from './formatters'

const memoryLabels: Record<GpuMemoryKind, string> = {
    dedicated: 'VRAM',
    unified: 'Unified memory',
    shared: 'Shared memory',
    unavailable: 'Memory unavailable'
}

export function getGpuMemoryLabel(kind: GpuMemoryKind): string {
    return memoryLabels[kind]
}

function bytes(value: number | null): string {
    return value === null ? '—' : formatBytes(value, 1)
}

// Percentages describe only this adapter's primary memory pool. Windows shared
// usage is already bytes and must never be multiplied by the dedicated total.
export function getGpuMemoryPresentation(
    gpu: GpuInfo,
    percent: number | null,
    sharedUsed: number | null
) {
    const label = getGpuMemoryLabel(gpu.memoryKind)
    if (gpu.memoryKind === 'unavailable') return { label, detail: null, sharedDetail: null }

    const used = bytes(
        gpu.vramTotal === null || percent === null ? null : (gpu.vramTotal * percent) / 100
    )
    const total = bytes(gpu.vramTotal)
    const detail =
        gpu.memoryKind === 'unified'
            ? `${used} used of ${total} unified memory`
            : `${used} / ${total}`
    const sharedDetail =
        gpu.memoryKind === 'dedicated' && gpu.sharedTotal !== null
            ? `Shared memory ${bytes(sharedUsed)} / ${bytes(gpu.sharedTotal)}`
            : null
    return { label, detail, sharedDetail }
}
