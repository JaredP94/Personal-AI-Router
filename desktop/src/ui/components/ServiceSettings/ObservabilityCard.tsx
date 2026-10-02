// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useState } from 'react'
import { Badge, Button, Flex, Stack, Switch, Text } from '@nvidia/foundations-react-core'
import type { TelemetryStatus } from '@/shared/types/telemetry'
import {
    getObservabilityBadge,
    getPhoenixDashboardUrl,
    isDockerUnavailable,
    isPhoenixRunning
} from '@/shared/utils/telemetry'
import getErrorString from '@/shared/utils/get-error-string'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'
import { OpenInNew } from '@/ui/components/icons'

export default function ObservabilityCard() {
    const [status, setStatus] = useState<TelemetryStatus | null>(null)
    const [error, setError] = useState<string | null>(null)
    const [loading, setLoading] = useState<'start' | 'stop' | 'update' | 'check' | null>(null)
    const [dismissedUpdate, setDismissedUpdate] = useState<string | null>(null)

    const fetchStatus = useCallback(async () => {
        if (!window.pairApi?.telemetry) return
        try {
            const nextStatus = await window.pairApi.telemetry.getStatus()
            setStatus(nextStatus)
        } catch {
            // Ignore background polling errors so user-triggered error states are preserved
        }
    }, [])

    useEffect(() => {
        let unmounted = false
        const interval =
            status?.containerState === 'starting' || status?.containerState === 'updating'
                ? 1500
                : 4000

        const poll = async () => {
            if (unmounted || !window.pairApi?.telemetry) return
            try {
                const nextStatus = await window.pairApi.telemetry.getStatus()
                if (!unmounted) {
                    setStatus(nextStatus)
                }
            } catch {
                // Ignore background polling errors
            }
        }

        void poll()
        const intervalId = setInterval(() => {
            void poll()
        }, interval)

        return () => {
            unmounted = true
            clearInterval(intervalId)
        }
    }, [status?.containerState])

    const handleStart = useCallback(async () => {
        if (!window.pairApi?.telemetry) return
        setLoading('start')
        setError(null)
        try {
            const nextStatus = await window.pairApi.telemetry.start()
            setStatus(nextStatus)
        } catch (err) {
            setError(getErrorString(err))
        } finally {
            setLoading(null)
        }
    }, [])

    const handleStop = useCallback(async () => {
        if (!window.pairApi?.telemetry) return
        setLoading('stop')
        setError(null)
        try {
            const nextStatus = await window.pairApi.telemetry.stop()
            setStatus(nextStatus)
        } catch (err) {
            setError(getErrorString(err))
        } finally {
            setLoading(null)
        }
    }, [])

    const handleCheckUpdate = useCallback(async () => {
        if (!window.pairApi?.telemetry) return
        setLoading('check')
        setError(null)
        try {
            const nextStatus = await window.pairApi.telemetry.checkUpdate()
            setStatus(nextStatus)
        } catch (err) {
            setError(getErrorString(err))
        } finally {
            setLoading(null)
        }
    }, [])

    const handleUpdate = useCallback(async () => {
        if (!window.pairApi?.telemetry) return
        setLoading('update')
        setError(null)
        try {
            const nextStatus = await window.pairApi.telemetry.update()
            setStatus(nextStatus)
        } catch (err) {
            setError(getErrorString(err))
        } finally {
            setLoading(null)
        }
    }, [])

    const handleOpenDashboard = useCallback(() => {
        const url = getPhoenixDashboardUrl(status)
        if (window.windowApi?.window?.openExternal) {
            void window.windowApi.window.openExternal(url).catch(() => {})
        } else if (typeof window.open === 'function') {
            window.open(url, '_blank', 'noopener,noreferrer')
        }
    }, [status])

    const handleToggleEnabled = useCallback(
        async (checked: boolean) => {
            if (!window.pairApi?.telemetry) return
            setStatus(prev => (prev ? { ...prev, enabled: checked } : null))
            try {
                await window.pairApi.telemetry.setEnabled(checked)
            } catch (err) {
                setError(getErrorString(err))
                void fetchStatus()
            }
        },
        [fetchStatus]
    )

    const handleToggleRecordPayloads = useCallback(
        async (checked: boolean) => {
            if (!window.pairApi?.telemetry) return
            setStatus(prev => (prev ? { ...prev, recordPayloads: checked } : null))
            try {
                await window.pairApi.telemetry.setRecordPayloads(checked)
            } catch (err) {
                setError(getErrorString(err))
                void fetchStatus()
            }
        },
        [fetchStatus]
    )

    const isRunning = isPhoenixRunning(status)
    const dockerUnavailable = isDockerUnavailable(status)
    const isUpdating = status?.containerState === 'updating' || loading === 'update'
    const { label: statusLabel, color: statusColor } = getObservabilityBadge(status)

    const showUpdateBanner =
        Boolean(status?.updateAvailable) &&
        !dockerUnavailable &&
        dismissedUpdate !== (status?.latestVersion ?? 'latest')

    return (
        <div className="settings-card settings-card-stacked pair-paper p-4">
            <Stack gap="4">
                {dockerUnavailable && (
                    <InlineErrorBanner
                        severity="warning"
                        message="Docker is not running or unavailable. Please ensure Docker Desktop is started to run Arize Phoenix."
                    />
                )}
                {showUpdateBanner && (
                    <InlineErrorBanner
                        severity="info"
                        message={`A newer Arize Phoenix image (${status?.latestVersion ?? 'latest'}) is available. Update now to pull and recreate the container with the latest image.`}
                        onClose={() => setDismissedUpdate(status?.latestVersion ?? 'latest')}
                    />
                )}
                {error && (
                    <InlineErrorBanner
                        severity="error"
                        message={error}
                        onClose={() => setError(null)}
                    />
                )}

                <Stack gap="1">
                    <Flex justify="between" align="center" gap="4" wrap="wrap">
                        <Flex align="center" gap="3" wrap="wrap">
                            <Text kind="body/semibold/md">
                                Observability & Evaluations (Arize Phoenix)
                            </Text>
                            <Badge color={statusColor} kind="solid">
                                {statusLabel}
                            </Badge>
                            {status?.currentVersion && (
                                <Badge color="gray" kind="outline">
                                    {status.currentVersion}
                                </Badge>
                            )}
                        </Flex>

                        <Flex gap="2" wrap="wrap">
                            {status?.updateAvailable && !dockerUnavailable && (
                                <Button
                                    kind="primary"
                                    size="small"
                                    onClick={handleUpdate}
                                    disabled={isUpdating}
                                >
                                    {isUpdating
                                        ? 'Updating…'
                                        : `Update Phoenix (${status.latestVersion ?? 'latest'})`}
                                </Button>
                            )}
                            {isRunning ? (
                                <>
                                    <Button
                                        kind="secondary"
                                        size="small"
                                        onClick={handleStop}
                                        disabled={loading === 'stop' || isUpdating}
                                    >
                                        {loading === 'stop' ? 'Stopping…' : 'Stop Phoenix'}
                                    </Button>
                                    <Button
                                        kind="secondary"
                                        size="small"
                                        onClick={handleOpenDashboard}
                                        disabled={!status?.collectorReachable || isUpdating}
                                    >
                                        <Flex align="center" gap="1">
                                            <span>Open Dashboard</span>
                                            <OpenInNew style={{ fontSize: 14 }} />
                                        </Flex>
                                    </Button>
                                </>
                            ) : (
                                <Button
                                    kind="secondary"
                                    size="small"
                                    onClick={handleStart}
                                    disabled={
                                        loading === 'start' || dockerUnavailable || isUpdating
                                    }
                                >
                                    {loading === 'start' ? 'Starting…' : 'Start Phoenix'}
                                </Button>
                            )}
                            {!dockerUnavailable && (
                                <Button
                                    kind="tertiary"
                                    size="small"
                                    onClick={handleCheckUpdate}
                                    disabled={loading === 'check' || isUpdating}
                                >
                                    {loading === 'check' ? 'Checking…' : 'Check for Updates'}
                                </Button>
                            )}
                        </Flex>
                    </Flex>

                    <Text kind="body/regular/sm" className="text-subtle-color">
                        Distributed OpenTelemetry tracing and evaluation dashboard for AI cluster
                        inference.
                    </Text>
                </Stack>

                <Stack gap="3">
                    <Flex justify="between" align="center" gap="4">
                        <Stack gap="1" className="flex-1">
                            <Text kind="body/semibold/sm">OpenTelemetry Tracing</Text>
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                Emit distributed traces across cluster nodes for inference requests.
                            </Text>
                        </Stack>
                        <Switch
                            size="small"
                            checked={status?.enabled ?? true}
                            disabled={status === null}
                            onCheckedChange={handleToggleEnabled}
                            aria-label="OpenTelemetry Tracing"
                        />
                    </Flex>

                    <Flex justify="between" align="start" gap="4">
                        <Stack gap="1" className="flex-1">
                            <Text kind="body/semibold/sm">
                                Capture Prompt & Completion Text (For Evaluations)
                            </Text>
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                Attach prompt and generated model response text to local spans for
                                quality evaluation, drift analysis, and model comparison.
                            </Text>
                            <Text kind="body/regular/xs" className="text-subtle-color mt-1">
                                Evaluations require capturing inference text to your local Phoenix
                                instance. Sensitive auth headers, keys, and tokens are permanently
                                redacted.
                            </Text>
                        </Stack>
                        <Switch
                            size="small"
                            checked={status?.recordPayloads ?? false}
                            disabled={status === null}
                            onCheckedChange={handleToggleRecordPayloads}
                            aria-label="Capture Prompt & Completion Text (For Evaluations)"
                        />
                    </Flex>
                </Stack>
            </Stack>
        </div>
    )
}
