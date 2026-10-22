// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { NodeStatuses } from '@/shared/constants/nodes'
import { SystemTopology } from '@/shared/types/hardware'
import type { EngineNativeView } from '@/shared/types/metrics'
import { PlatformDisplayName } from '@/shared/types/platform'

export type NodeStatus = (typeof NodeStatuses)[number]

export interface NodeItem {
    id: string
    name: string
    status: NodeStatus
    ipAddress: string
    port: number
    allIpAddresses: string[]
    topology: SystemTopology
    os: PlatformDisplayName
    /**
     * The engine's own view of itself, as the engine published it.
     *
     * Absent means the engine has no native view — it publishes no admin API,
     * or it could not be reached and never produced one. It is the engine's
     * own account of its work, so it includes traffic PAIR did not route and
     * is shown beside PAIR's own counters, never merged into them.
     */
    engineNative?: EngineNativeView
}
