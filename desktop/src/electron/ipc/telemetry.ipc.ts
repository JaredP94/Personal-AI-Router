// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { safeHandle } from '@/electron/ipc/safe-handle'
import {
    handleTelemetryGetStatus,
    handleTelemetryStart,
    handleTelemetryStop,
    handleTelemetryCheckUpdate,
    handleTelemetryUpdate,
    handleTelemetrySetEnabled,
    handleTelemetrySetEndpoint,
    handleTelemetrySetRecordPayloads
} from '@/electron/service-bridge/empty-handlers'

export function registerTelemetryIpc(): void {
    safeHandle('telemetry:get-status', () => handleTelemetryGetStatus())
    safeHandle('telemetry:start', () => handleTelemetryStart())
    safeHandle('telemetry:stop', () => handleTelemetryStop())
    safeHandle('telemetry:check-update', () => handleTelemetryCheckUpdate())
    safeHandle('telemetry:update', () => handleTelemetryUpdate())
    safeHandle('telemetry:set-enabled', (_event, payload) => handleTelemetrySetEnabled(payload))
    safeHandle('telemetry:set-endpoint', (_event, payload) => handleTelemetrySetEndpoint(payload))
    safeHandle('telemetry:set-record-payloads', (_event, payload) =>
        handleTelemetrySetRecordPayloads(payload)
    )
}
