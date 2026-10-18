// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Badge, Button, Flex, Stack, Switch, Text, TextInput } from '@nvidia/foundations-react-core'
import type { TelemetryConfigRequest, TelemetryStatus } from '@/shared/types/telemetry'
import {
    getObservabilityBadge,
    getPhoenixDashboardUrl,
    isDockerUnavailable,
    isPhoenixRunning
} from '@/shared/utils/telemetry'
import getErrorString from '@/shared/utils/get-error-string'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'
import { ConfirmModal } from '@/ui/components/ConfirmModal'
import { OpenInNew } from '@/ui/components/icons'
import {
    formatTelemetryStorage,
    isCollectorPortValid,
    observabilityCopy,
    observabilityWarnings
} from '@/ui/utils/observability-copy'
import { createTelemetryPoller, type TelemetryPollCadence } from '@/ui/utils/telemetry-poller'

export default function ObservabilityCard() {
    const [status, setStatus] = useState<TelemetryStatus | null>(null)
    const [error, setError] = useState<string | null>(null)
    const [loading, setLoading] = useState<'start' | 'stop' | 'purge' | 'config' | null>(null)
    const [consentModalOpen, setConsentModalOpen] = useState(false)
    const [clearModalOpen, setClearModalOpen] = useState(false)

    // Collector port input state
    const [portInput, setPortInput] = useState<string>('4317')
    const [portEdited, setPortEdited] = useState(false)

    const statusLoadedRef = useRef(false)

    const fetchStatus = useCallback(async () => {
        if (!window.pairApi?.telemetry) return
        try {
            const nextStatus = await window.pairApi.telemetry.getStatus()
            statusLoadedRef.current = true
            setStatus(nextStatus)
        } catch (err) {
            if (!statusLoadedRef.current) {
                setError(getErrorString(err))
            }
        }
    }, [])

    const cadence: TelemetryPollCadence =
        status?.containerState === 'starting' ? 'starting' : 'active'

    const poller = useMemo(
        () => createTelemetryPoller(fetchStatus, c => (c === 'starting' ? 1500 : 4000)),
        [fetchStatus]
    )

    useEffect(() => {
        poller.setCadence(cadence)
    }, [poller, cadence])

    useEffect(() => {
        return () => {
            poller.stop()
        }
    }, [poller])

    useEffect(() => {
        if (!portEdited && status?.collectorPort != null) {
            setPortInput(String(status.collectorPort))
        }
    }, [status?.collectorPort, portEdited])

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

    const handleOpenDashboard = useCallback(() => {
        const url = getPhoenixDashboardUrl(status)
        if (window.windowApi?.window?.openExternal) {
            void window.windowApi.window.openExternal(url).catch(() => {})
        } else if (typeof window.open === 'function') {
            window.open(url, '_blank', 'noopener,noreferrer')
        }
    }, [status])

    const handleUpdateConfig = useCallback(
        async (config: TelemetryConfigRequest) => {
            if (!window.pairApi?.telemetry) return
            setLoading('config')
            setError(null)
            try {
                await window.pairApi.telemetry.setConfig(config)
                await fetchStatus()
            } catch (err) {
                setError(getErrorString(err))
                void fetchStatus()
            } finally {
                setLoading(null)
            }
        },
        [fetchStatus]
    )

    const handleToggleEnabled = useCallback(
        async (checked: boolean) => {
            const currentPort = status?.collectorPort ?? 4317
            await handleUpdateConfig({
                enabled: checked,
                collectorPort: currentPort,
                recordPayloads: false,
                payloadConsentVersion: 0
            })
        },
        [handleUpdateConfig, status?.collectorPort]
    )

    const handleToggleRecordPayloads = useCallback(
        async (checked: boolean) => {
            if (checked) {
                setConsentModalOpen(true)
                return
            }
            const currentPort = status?.collectorPort ?? 4317
            await handleUpdateConfig({
                enabled: status?.enabled ?? true,
                collectorPort: currentPort,
                recordPayloads: false,
                payloadConsentVersion: 0
            })
        },
        [handleUpdateConfig, status?.collectorPort, status?.enabled]
    )

    const handleConfirmConsent = useCallback(async () => {
        if (!status) return
        await handleUpdateConfig({
            enabled: status.enabled,
            collectorPort: status.collectorPort,
            recordPayloads: true,
            payloadConsentVersion: status.payloadPolicyVersion
        })
    }, [handleUpdateConfig, status])

    const handleApplyPort = useCallback(async () => {
        if (!status) return
        if (!isCollectorPortValid(portInput)) return
        const portNum = Number(portInput.trim())
        setPortEdited(false)
        await handleUpdateConfig({
            enabled: status.enabled,
            collectorPort: portNum,
            recordPayloads: status.recordPayloads,
            payloadConsentVersion: status.recordPayloads ? status.payloadConsentVersion : 0
        })
    }, [handleUpdateConfig, portInput, status])

    const handlePurgeStorage = useCallback(async () => {
        if (!window.pairApi?.telemetry) return
        setLoading('purge')
        setError(null)
        try {
            const result = await window.pairApi.telemetry.purgeStorage({ confirm: 'delete-traces' })
            if (result.refused.length > 0) {
                setError(
                    `Could not clear some trace volumes: ${result.refused.map(r => `${r.name}: ${r.reason}`).join(', ')}`
                )
            }
            await fetchStatus()
        } catch (err) {
            setError(getErrorString(err))
        } finally {
            setLoading(null)
        }
    }, [fetchStatus])

    const isRunning = isPhoenixRunning(status)
    const dockerUnavailable = isDockerUnavailable(status)
    const { label: statusLabel, color: statusColor } = getObservabilityBadge(status)
    const warnings = observabilityWarnings(status)

    const portIsValid = isCollectorPortValid(portInput)
    const portChanged =
        status != null && portIsValid && Number(portInput.trim()) !== status.collectorPort
    const showPortHint = portInput.trim() !== '' && !portIsValid

    return (
        <div className="settings-card settings-card-stacked pair-paper p-4">
            <Stack gap="4">
                {dockerUnavailable && (
                    <InlineErrorBanner
                        severity="warning"
                        message="Docker is not running or unavailable. Please ensure Docker Desktop is started to run Arize Phoenix."
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
                        </Flex>

                        <Flex gap="2" wrap="wrap">
                            {isRunning ? (
                                <>
                                    <Button
                                        kind="secondary"
                                        size="small"
                                        onClick={handleStop}
                                        disabled={loading === 'stop'}
                                    >
                                        {loading === 'stop' ? 'Stopping…' : 'Stop Phoenix'}
                                    </Button>
                                    <Button
                                        kind="secondary"
                                        size="small"
                                        onClick={handleOpenDashboard}
                                        disabled={!status?.collectorReachable}
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
                                    disabled={loading === 'start' || dockerUnavailable}
                                >
                                    {loading === 'start' ? 'Starting…' : 'Start Phoenix'}
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
                            checked={status?.enabled ?? false}
                            disabled={status === null || loading !== null}
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
                            disabled={status === null || !status.enabled || loading !== null}
                            onCheckedChange={handleToggleRecordPayloads}
                            aria-label="Capture Prompt & Completion Text (For Evaluations)"
                        />
                    </Flex>

                    <Flex justify="between" align="center" gap="4" wrap="wrap">
                        <Stack gap="1" className="flex-1">
                            <Text kind="body/semibold/sm">Collector Port</Text>
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                Loopback port where Phoenix collector accepts OTLP trace data
                                (1–65535).
                            </Text>
                            {showPortHint && (
                                <Text kind="body/regular/xs" className="text-danger-color">
                                    Port must be a valid number between 1 and 65535.
                                </Text>
                            )}
                        </Stack>
                        <Flex align="center" gap="2">
                            <TextInput
                                size="small"
                                value={portInput}
                                disabled={status === null || loading !== null}
                                onValueChange={val => {
                                    setPortEdited(true)
                                    setPortInput(val)
                                }}
                                onKeyDown={e => {
                                    if (e.key === 'Enter' && portChanged) {
                                        void handleApplyPort()
                                    }
                                }}
                                className="w-24 min-w-24"
                                aria-label="Collector Port"
                            />
                            {portChanged && (
                                <Button
                                    kind="primary"
                                    size="small"
                                    onClick={handleApplyPort}
                                    disabled={!portIsValid || loading !== null}
                                >
                                    Apply
                                </Button>
                            )}
                        </Flex>
                    </Flex>

                    {status?.workers && status.workers.length > 0 && (
                        <Stack gap="2" className="pt-2 border-t border-subtle">
                            <Text kind="body/semibold/sm">Worker Status</Text>
                            <Stack gap="1">
                                {status.workers.map(worker => (
                                    <Flex
                                        key={worker.name}
                                        justify="between"
                                        align="center"
                                        gap="4"
                                    >
                                        <Text kind="body/regular/sm">{worker.name}</Text>
                                        <Badge
                                            kind="outline"
                                            color={
                                                worker.state === 'applied'
                                                    ? 'green'
                                                    : worker.state === 'failed'
                                                      ? 'red'
                                                      : worker.state === 'pending'
                                                        ? 'yellow'
                                                        : 'gray'
                                            }
                                        >
                                            {worker.state.charAt(0).toUpperCase() +
                                                worker.state.slice(1)}
                                        </Badge>
                                    </Flex>
                                ))}
                            </Stack>
                        </Stack>
                    )}

                    <Flex
                        justify="between"
                        align="center"
                        gap="4"
                        wrap="wrap"
                        className="pt-2 border-t border-subtle"
                    >
                        <Stack gap="1" className="flex-1">
                            <Text kind="body/semibold/sm">Storage</Text>
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                {formatTelemetryStorage(status?.storage)}
                            </Text>
                        </Stack>
                        <Button
                            kind="secondary"
                            size="small"
                            onClick={() => setClearModalOpen(true)}
                            disabled={status === null || loading === 'purge'}
                        >
                            {loading === 'purge' ? 'Clearing…' : 'Clear stored traces'}
                        </Button>
                    </Flex>
                </Stack>

                {warnings.length > 0 && (
                    <Stack gap="2" className="pt-2">
                        {warnings.map(warning => (
                            <InlineErrorBanner key={warning} severity="warning" message={warning} />
                        ))}
                    </Stack>
                )}
            </Stack>

            <ConfirmModal
                open={consentModalOpen}
                onOpenChange={setConsentModalOpen}
                title={observabilityCopy.consent.title}
                message={observabilityCopy.consent.body}
                confirmLabel={observabilityCopy.consent.confirm}
                confirmColor="brand"
                onConfirm={handleConfirmConsent}
            />

            <ConfirmModal
                open={clearModalOpen}
                onOpenChange={setClearModalOpen}
                title={observabilityCopy.clear.title}
                message={observabilityCopy.clear.body}
                confirmLabel={observabilityCopy.clear.confirm}
                confirmColor="danger"
                onConfirm={handlePurgeStorage}
            />
        </div>
    )
}
