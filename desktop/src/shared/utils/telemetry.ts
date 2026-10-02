// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { TelemetryStatus } from '@/shared/types/telemetry'

type ObservabilityBadgeColor = 'green' | 'yellow' | 'gray'

interface ObservabilityBadge {
    label: string
    color: ObservabilityBadgeColor
}

/**
 * Returns the status pill label and color according to Phoenix collector and container state:
 * - Active ● (green) if collector is reachable
 * - Starting… (yellow) if container is running but collector not yet reachable
 * - Stopped (gray) otherwise
 */
export function getObservabilityBadge(status: TelemetryStatus | null): ObservabilityBadge {
    if (status?.containerState === 'updating') {
        return { label: 'Updating…', color: 'yellow' }
    }
    if (status?.collectorReachable) {
        return { label: 'Active ●', color: 'green' }
    }
    if (status?.containerState === 'running' || status?.containerState === 'starting') {
        return { label: 'Starting…', color: 'yellow' }
    }
    return { label: 'Stopped', color: 'gray' }
}

/**
 * True if Phoenix collector is reachable or container is running / starting / updating.
 */
export function isPhoenixRunning(status: TelemetryStatus | null): boolean {
    return Boolean(
        status?.collectorReachable ||
        status?.containerState === 'running' ||
        status?.containerState === 'starting' ||
        status?.containerState === 'updating'
    )
}

/**
 * True if Docker is reported as unavailable by the service.
 */
export function isDockerUnavailable(status: TelemetryStatus | null): boolean {
    return (
        status?.containerState === 'docker-unavailable' ||
        status?.containerState === 'docker_unavailable'
    )
}

/**
 * Resolves the Phoenix dashboard UI URL, defaulting to http://localhost:6006.
 */
export function getPhoenixDashboardUrl(status: TelemetryStatus | null): string {
    return status?.uiUrl || 'http://localhost:6006'
}
