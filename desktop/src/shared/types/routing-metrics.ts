// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { EngineType } from '@/shared/types/engines'

export interface RoutingMetrics {
    engineType: EngineType
    requests: number
    cacheAffinityHits: number
}
