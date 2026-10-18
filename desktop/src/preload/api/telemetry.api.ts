// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { ipcRenderer } from 'electron'
import { invokeAndUnwrap } from '@/preload/api/unwrap'
import type {
    TelemetryConfig,
    TelemetryConfigRequest,
    TelemetryPurgeRequest,
    TelemetryPurgeResult,
    TelemetryStatus
} from '@/shared/types/telemetry'

export interface ITelemetryApi {
    getStatus(): Promise<TelemetryStatus>
    start(): Promise<TelemetryStatus>
    stop(): Promise<TelemetryStatus>
    setConfig(config: TelemetryConfigRequest): Promise<TelemetryConfig>
    purgeStorage(request: TelemetryPurgeRequest): Promise<TelemetryPurgeResult>
}

export const telemetryApi: ITelemetryApi = {
    getStatus: () => invokeAndUnwrap<TelemetryStatus>(ipcRenderer.invoke('telemetry:get-status')),
    start: () => invokeAndUnwrap<TelemetryStatus>(ipcRenderer.invoke('telemetry:start')),
    stop: () => invokeAndUnwrap<TelemetryStatus>(ipcRenderer.invoke('telemetry:stop')),
    setConfig: config =>
        invokeAndUnwrap<TelemetryConfig>(ipcRenderer.invoke('telemetry:set-config', config)),
    purgeStorage: request =>
        invokeAndUnwrap<TelemetryPurgeResult>(
            ipcRenderer.invoke('telemetry:purge-storage', request)
        )
}
