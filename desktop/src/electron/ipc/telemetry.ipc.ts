// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { safeHandle } from '@/electron/ipc/safe-handle'
import {
    handleTelemetryGetStatus,
    handleTelemetryStart,
    handleTelemetryStop,
    handleTelemetrySetConfig,
    handleTelemetryPurgeStorage
} from '@/electron/service-bridge/empty-handlers'

export function registerTelemetryIpc(): void {
    safeHandle('telemetry:get-status', () => handleTelemetryGetStatus())
    safeHandle('telemetry:start', () => handleTelemetryStart())
    safeHandle('telemetry:stop', () => handleTelemetryStop())
    safeHandle('telemetry:set-config', (_event, payload) => handleTelemetrySetConfig(payload))
    safeHandle('telemetry:purge-storage', (_event, payload) => handleTelemetryPurgeStorage(payload))
}
