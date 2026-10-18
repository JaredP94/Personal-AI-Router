// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export interface TelemetryConfigRequest {
    enabled: boolean
    collectorPort: number
    recordPayloads: boolean
    payloadConsentVersion: number
}

export interface TelemetryConfig extends TelemetryConfigRequest {
    revision: number
    payloadPolicyVersion: number
}

export interface TelemetryStorage {
    bytes: number | null
    volumes: { name: string; bytes: number | null }[]
    measuredAt: string | null
    error: string | null
}

export interface TelemetryStatus extends TelemetryConfig {
    workers: {
        name: string
        appliedRevision: number
        state: string
        error: string
        lastExportOk: string | null
        lastExportError: string | null
        lastExportErrorAt: string | null
    }[]
    containerState: string
    collectorReachable: boolean
    uiUrl: string
    imageVersion: string
    bindingsLoopbackOnly: boolean | null
    managedConfig: 'current' | 'outdated' | 'missing'
    storage: TelemetryStorage
}

export interface TelemetryPurgeRequest {
    confirm: 'delete-traces'
}

export interface TelemetryPurgeResult {
    removed: string[]
    refused: { name: string; reason: string }[]
    storage: TelemetryStorage
}
