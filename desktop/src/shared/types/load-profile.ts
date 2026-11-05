// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export interface LoadProfileTarget {
    nodeId: string
    engine: 'lmstudio'
    model: string
}

export interface LoadProfileRequest extends LoadProfileTarget {
    expectedRevision: number
    requestId?: string
    loadText: string
    format?: string
}

export interface LoadProfileSnapshot extends LoadProfileTarget {
    revision: number
    loadText: string
    format: string
    editable: boolean
    reason: string
    /** Advisory residency only; never grants ownership or automatic unload permission. */
    loadState?: string
}

export interface LoadProfilePreview {
    revision: number
    loadText: string
    format: string
    load_args: string[]
    errors: Record<string, string>
}
