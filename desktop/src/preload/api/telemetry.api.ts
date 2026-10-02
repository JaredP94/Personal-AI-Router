// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { ipcRenderer } from 'electron'
import { invokeAndUnwrap } from '@/preload/api/unwrap'
import type { TelemetryStatus } from '@/shared/types/telemetry'

export interface ITelemetryApi {
    getStatus(): Promise<TelemetryStatus>
    start(): Promise<TelemetryStatus>
    stop(): Promise<TelemetryStatus>
    checkUpdate(): Promise<TelemetryStatus>
    update(): Promise<TelemetryStatus>
    setEnabled(value: boolean): Promise<{ ok: boolean }>
    setEndpoint(value: string): Promise<{ ok: boolean }>
    setRecordPayloads(value: boolean): Promise<{ ok: boolean }>
}

export const telemetryApi: ITelemetryApi = {
    getStatus: () => invokeAndUnwrap<TelemetryStatus>(ipcRenderer.invoke('telemetry:get-status')),
    start: () => invokeAndUnwrap<TelemetryStatus>(ipcRenderer.invoke('telemetry:start')),
    stop: () => invokeAndUnwrap<TelemetryStatus>(ipcRenderer.invoke('telemetry:stop')),
    checkUpdate: () =>
        invokeAndUnwrap<TelemetryStatus>(ipcRenderer.invoke('telemetry:check-update')),
    update: () => invokeAndUnwrap<TelemetryStatus>(ipcRenderer.invoke('telemetry:update')),
    setEnabled: value =>
        invokeAndUnwrap<{ ok: boolean }>(ipcRenderer.invoke('telemetry:set-enabled', { value })),
    setEndpoint: value =>
        invokeAndUnwrap<{ ok: boolean }>(ipcRenderer.invoke('telemetry:set-endpoint', { value })),
    setRecordPayloads: value =>
        invokeAndUnwrap<{ ok: boolean }>(
            ipcRenderer.invoke('telemetry:set-record-payloads', { value })
        )
}
