// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export type IncomingSyncRow = {
    engineType?: import('@/shared/types/engines').EngineType
    opId?: string
    canCancel?: boolean
    rawModel: string
    label: string
    status: string
    percent?: number
    completed?: number
    total?: number
}
