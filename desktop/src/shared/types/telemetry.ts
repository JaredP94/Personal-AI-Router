// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export interface TelemetryStatus {
    enabled: boolean
    endpoint: string
    uiUrl: string
    recordPayloads: boolean
    containerState: string
    collectorReachable: boolean
    currentVersion?: string
    updateAvailable?: boolean
    latestVersion?: string
}
