// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export type WorkerState = 'running' | 'degraded' | 'restarting' | 'exhausted' | 'missing'

export type WorkerStatus = {
    name: string
    state: WorkerState
    attempts: number
    pid?: number
    lastError?: string
    restartOnHang: boolean
    since: string
}
