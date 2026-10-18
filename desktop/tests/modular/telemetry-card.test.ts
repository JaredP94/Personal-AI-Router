// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { TelemetryStatus } from '@/shared/types/telemetry'
import {
    formatTelemetryStorage,
    isCollectorPortValid,
    observabilityCopy,
    observabilityWarnings,
    scheduleTelemetryPolling
} from '@/ui/utils/observability-copy'
import {
    getObservabilityBadge,
    getPhoenixDashboardUrl,
    isDockerUnavailable,
    isPhoenixRunning
} from '@/shared/utils/telemetry'

const mocks = vi.hoisted(() => ({
    supervisor: {
        hasProcess: vi.fn(() => true),
        callProcess: vi.fn(),
        sendProcess: vi.fn(),
        reportError: vi.fn()
    }
}))

vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => mocks.supervisor
}))
vi.mock('@/electron/service-bridge/modular-state', () => ({
    getModularBridgeState: () => ({ getSelfId: () => 'local-node' }),
    isProxyEngine: () => false,
    isUpstreamUnreachableError: () => false,
    parseServiceErrors: () => []
}))
vi.mock('@/electron/model-hub', () => ({ getEngineHubModels: vi.fn() }))

import { handleServiceBridgeInvoke } from '@/electron/service-bridge/empty-handlers'

function telemetryStatus(overrides: Partial<TelemetryStatus> = {}): TelemetryStatus {
    return {
        revision: 4,
        enabled: true,
        collectorPort: 4317,
        recordPayloads: false,
        payloadConsentVersion: 0,
        payloadPolicyVersion: 1,
        workers: [
            {
                name: 'proxy',
                appliedRevision: 4,
                state: 'applied',
                error: '',
                lastExportOk: '2026-10-18T12:00:00Z',
                lastExportError: null,
                lastExportErrorAt: null
            }
        ],
        containerState: 'running',
        collectorReachable: true,
        uiUrl: 'http://127.0.0.1:6006',
        imageVersion: 'version-20.23.0',
        bindingsLoopbackOnly: true,
        managedConfig: 'current',
        storage: {
            bytes: 1024,
            volumes: [{ name: 'nvpair-telemetry_nvpair-phoenix-data', bytes: 1024 }],
            measuredAt: '2026-10-18T12:00:00Z',
            error: null
        },
        ...overrides
    }
}

describe('telemetry service bridge', () => {
    beforeEach(() => {
        mocks.supervisor.callProcess.mockReset().mockResolvedValue(undefined)
    })

    it('preserves the complete authoritative broker status including nullable fields', async () => {
        const status = telemetryStatus({ bindingsLoopbackOnly: null, managedConfig: 'missing' })
        mocks.supervisor.callProcess.mockResolvedValueOnce(status)
        await expect(handleServiceBridgeInvoke('telemetry:get-status', undefined)).resolves.toEqual(
            status
        )
        expect(mocks.supervisor.callProcess).toHaveBeenCalledWith('broker', 'telemetry/get-status')
    })

    it('surfaces status errors without attempting settings fallback', async () => {
        mocks.supervisor.callProcess.mockRejectedValueOnce(new Error('broker unavailable'))
        await expect(handleServiceBridgeInvoke('telemetry:get-status', undefined)).rejects.toThrow(
            'broker unavailable'
        )
        expect(mocks.supervisor.callProcess).toHaveBeenCalledTimes(1)
    })

    it.each(['telemetry:start', 'telemetry:stop'])(
        'returns the full status for %s',
        async channel => {
            const status = telemetryStatus({ containerState: 'stopped', collectorReachable: false })
            mocks.supervisor.callProcess.mockResolvedValueOnce(status)
            if (channel === 'telemetry:start') {
                await expect(
                    handleServiceBridgeInvoke('telemetry:start', undefined)
                ).resolves.toEqual(status)
                expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                    'broker',
                    'telemetry/start',
                    undefined,
                    15_000
                )
            } else {
                await expect(
                    handleServiceBridgeInvoke('telemetry:stop', undefined)
                ).resolves.toEqual(status)
                expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                    'broker',
                    'telemetry/stop',
                    undefined,
                    15_000
                )
            }
        }
    )

    it.each([
        { recordPayloads: true, consent: 1 },
        { recordPayloads: false, consent: 0 }
    ])(
        'forwards consent only while capture is $recordPayloads',
        async ({ recordPayloads, consent }) => {
            const config = {
                revision: 5,
                enabled: true,
                collectorPort: 54317,
                recordPayloads,
                payloadConsentVersion: consent,
                payloadPolicyVersion: 1
            }
            mocks.supervisor.callProcess.mockResolvedValueOnce(config)
            await expect(
                handleServiceBridgeInvoke('telemetry:set-config', {
                    enabled: true,
                    collectorPort: 54317,
                    recordPayloads,
                    payloadConsentVersion: 1
                })
            ).resolves.toEqual(config)
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'settings/set-telemetry-config',
                {
                    enabled: true,
                    collectorPort: 54317,
                    recordPayloads,
                    payloadConsentVersion: consent
                }
            )
        }
    )

    it('relays purge confirmation and preserves refused volumes and unknown storage sizes', async () => {
        const result = {
            removed: ['unused-traces'],
            refused: [{ name: 'busy-traces', reason: 'volume is used by active container' }],
            storage: {
                bytes: null,
                volumes: [{ name: 'busy-traces', bytes: null }],
                measuredAt: null,
                error: 'size unavailable'
            }
        }
        mocks.supervisor.callProcess.mockResolvedValueOnce(result)
        await expect(
            handleServiceBridgeInvoke('telemetry:purge-storage', { confirm: 'delete-traces' })
        ).resolves.toEqual(result)
        expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
            'broker',
            'telemetry/purge-storage',
            { confirm: 'delete-traces' },
            120_000
        )
    })
})

describe('Observability privacy copy and warnings', () => {
    it('uses the approved consent and deletion wording verbatim', () => {
        expect(observabilityCopy).toEqual({
            consent: {
                title: 'Capture prompts and responses?',
                body: "PAIR will attach the text of prompts and model responses (up to 16 KB each) to traces sent to Phoenix on this machine. Captured text may contain personal or confidential information and stays in Phoenix's storage until you clear it. Turning capture off stops new capture but can't remove text already stored.",
                confirm: 'Capture text',
                cancel: 'Cancel'
            },
            clear: {
                title: 'Clear stored traces?',
                body: 'This permanently deletes the traces stored by Phoenix on this machine. Phoenix stops while the data is deleted.',
                confirm: 'Delete traces',
                cancel: 'Cancel'
            }
        })
    })

    it('warns when enabled tracing is dropping because Phoenix is stopped', () => {
        expect(
            observabilityWarnings(
                telemetryStatus({ containerState: 'stopped', collectorReachable: false })
            )
        ).toEqual([
            "Tracing is on, but Phoenix isn't running, so this node's traces are being dropped."
        ])
    })
    it('does not warn about dropping when tracing is off or Phoenix is running', () => {
        expect(
            observabilityWarnings(
                telemetryStatus({
                    enabled: false,
                    containerState: 'stopped',
                    collectorReachable: false
                })
            )
        ).toEqual([])
        expect(observabilityWarnings(telemetryStatus())).toEqual([])
        expect(observabilityWarnings(null)).toEqual([])
    })
    it('warns about exposed network bindings', () => {
        expect(observabilityWarnings(telemetryStatus({ bindingsLoopbackOnly: false }))).toEqual([
            'Phoenix is reachable from other machines on your network. Restart it to apply the local-only configuration.'
        ])
    })
    it('does not confuse unknown bindings or missing config with an exposed or outdated container', () => {
        expect(
            observabilityWarnings(
                telemetryStatus({ bindingsLoopbackOnly: null, managedConfig: 'missing' })
            )
        ).toEqual([])
    })
    it('warns about an outdated managed config', () => {
        expect(observabilityWarnings(telemetryStatus({ managedConfig: 'outdated' }))).toEqual([
            'Phoenix is running an older PAIR configuration. Restart it to apply the current one.'
        ])
    })
    it('lists failed workers individually alongside the other warnings', () => {
        expect(
            observabilityWarnings(
                telemetryStatus({
                    containerState: 'stopped',
                    collectorReachable: false,
                    bindingsLoopbackOnly: false,
                    managedConfig: 'outdated',
                    workers: [
                        {
                            name: 'proxy',
                            appliedRevision: 3,
                            state: 'failed',
                            error: 'timeout',
                            lastExportOk: null,
                            lastExportError: null,
                            lastExportErrorAt: null
                        },
                        {
                            name: 'omlx-proxy',
                            appliedRevision: 3,
                            state: 'pending',
                            error: '',
                            lastExportOk: null,
                            lastExportError: null,
                            lastExportErrorAt: null
                        },
                        {
                            name: 'lmstudio-proxy',
                            appliedRevision: 3,
                            state: 'failed',
                            error: 'timeout',
                            lastExportOk: null,
                            lastExportError: null,
                            lastExportErrorAt: null
                        }
                    ]
                })
            )
        ).toEqual([
            "Tracing is on, but Phoenix isn't running, so this node's traces are being dropped.",
            'Phoenix is reachable from other machines on your network. Restart it to apply the local-only configuration.',
            'Phoenix is running an older PAIR configuration. Restart it to apply the current one.',
            "Tracing settings didn't reach proxy. Restart PAIR's services from this page if it persists.",
            "Tracing settings didn't reach lmstudio-proxy. Restart PAIR's services from this page if it persists."
        ])
    })
})

describe('Phoenix status helpers', () => {
    it('defaults to stopped when status is unavailable', () => {
        expect(getObservabilityBadge(null)).toEqual({ label: 'Stopped', color: 'gray' })
        expect(isPhoenixRunning(null)).toBe(false)
    })
    it('reports reachable, starting and stopped collector states', () => {
        expect(getObservabilityBadge(telemetryStatus())).toEqual({
            label: 'Active ●',
            color: 'green'
        })
        expect(
            getObservabilityBadge(
                telemetryStatus({ containerState: 'starting', collectorReachable: false })
            )
        ).toEqual({ label: 'Starting…', color: 'yellow' })
        expect(
            getObservabilityBadge(
                telemetryStatus({ containerState: 'stopped', collectorReachable: false })
            )
        ).toEqual({ label: 'Stopped', color: 'gray' })
        expect(
            isPhoenixRunning(
                telemetryStatus({ containerState: 'starting', collectorReachable: false })
            )
        ).toBe(true)
        expect(
            isPhoenixRunning(
                telemetryStatus({ containerState: 'stopped', collectorReachable: false })
            )
        ).toBe(false)
    })
    it('uses the broker Docker state and local dashboard URL', () => {
        expect(isDockerUnavailable(telemetryStatus({ containerState: 'docker_unavailable' }))).toBe(
            true
        )
        expect(isDockerUnavailable(telemetryStatus())).toBe(false)
        expect(getPhoenixDashboardUrl(telemetryStatus())).toBe('http://127.0.0.1:6006')
    })
})

describe('Telemetry input validation and formatting', () => {
    it('validates numeric collector ports strictly between 1 and 65535', () => {
        expect(isCollectorPortValid('4317')).toBe(true)
        expect(isCollectorPortValid('1')).toBe(true)
        expect(isCollectorPortValid('65535')).toBe(true)
        expect(isCollectorPortValid('0')).toBe(false)
        expect(isCollectorPortValid('-1')).toBe(false)
        expect(isCollectorPortValid('65536')).toBe(false)
        expect(isCollectorPortValid('4317.5')).toBe(false)
        expect(isCollectorPortValid('')).toBe(false)
        expect(isCollectorPortValid('abc')).toBe(false)
        expect(isCollectorPortValid('4317abc')).toBe(false)
    })

    it('formats storage size and measurement time or reports unavailable', () => {
        expect(formatTelemetryStorage(null)).toBe('Storage size unavailable')
        expect(
            formatTelemetryStorage({ bytes: null, volumes: [], measuredAt: null, error: null })
        ).toBe('Storage size unavailable')
        expect(
            formatTelemetryStorage({ bytes: 1024, volumes: [], measuredAt: null, error: null })
        ).toBe('1 KB')
        expect(
            formatTelemetryStorage({
                bytes: 1048576,
                volumes: [],
                measuredAt: new Date(Date.now() - 60_000).toISOString(),
                error: null
            })
        ).toMatch(/^1 MB \(.+\)$/)
    })
})

describe('Telemetry polling schedule', () => {
    it('polls status on a fixed cadence (1 + floor(10000/4000) = 3 calls in 10s) without looping on status object identity', async () => {
        vi.useFakeTimers()
        try {
            let callCount = 0
            const fakeGetStatus = vi.fn(async () => {
                callCount++
                // Returning a fresh but equal object each time must not add calls
                return telemetryStatus({ containerState: 'running' })
            })

            let currentCadence = 4000
            let cleanup: () => void = () => {}

            // Proper effect dependency: effect depends only on cadence
            const mountOrUpdate = (cadence: number) => {
                cleanup()
                cleanup = scheduleTelemetryPolling(
                    fakeGetStatus,
                    nextStatus => {
                        const nextCadence = nextStatus.containerState === 'starting' ? 1500 : 4000
                        if (currentCadence !== nextCadence) {
                            currentCadence = nextCadence
                            mountOrUpdate(nextCadence)
                        }
                    },
                    undefined,
                    cadence
                )
            }

            mountOrUpdate(currentCadence)

            // Initial poll on mount at t = 0
            await vi.advanceTimersByTimeAsync(0)
            expect(callCount).toBe(1)

            // Advancing 10s with running container (4000ms cadence):
            // Exactly 1 initial + floor(10000 / 4000) = 3 calls (t = 0, 4000, 8000)
            await vi.advanceTimersByTimeAsync(10_000)
            expect(callCount).toBe(3)
            expect(fakeGetStatus).toHaveBeenCalledTimes(3)

            // Cleanup stops all further polling
            cleanup()
            await vi.advanceTimersByTimeAsync(10_000)
            expect(callCount).toBe(3)
        } finally {
            vi.useRealTimers()
        }
    })

    it('uses 1500ms cadence for starting containers and adapts to 4000ms when running', async () => {
        vi.useFakeTimers()
        try {
            let callCount = 0
            let state = 'starting'
            const fakeGetStatus = vi.fn(async () => {
                callCount++
                return telemetryStatus({ containerState: state })
            })

            let currentCadence = 1500
            let cleanup: () => void = () => {}

            const mountOrUpdate = (cadence: number) => {
                cleanup()
                cleanup = scheduleTelemetryPolling(
                    fakeGetStatus,
                    nextStatus => {
                        const nextCadence = nextStatus.containerState === 'starting' ? 1500 : 4000
                        if (currentCadence !== nextCadence) {
                            currentCadence = nextCadence
                            mountOrUpdate(nextCadence)
                        }
                    },
                    undefined,
                    cadence
                )
            }

            mountOrUpdate(currentCadence)

            // Initial poll (t = 0)
            await vi.advanceTimersByTimeAsync(0)
            expect(callCount).toBe(1)

            // 1500ms interval for starting: after 3000ms, exactly 1 + floor(3000 / 1500) = 3 calls (t = 0, 1500, 3000)
            await vi.advanceTimersByTimeAsync(3_000)
            expect(callCount).toBe(3)

            // State changes to running
            state = 'running'
            // Next tick at t = 4500 (call 4), which detects state transition to running.
            // On cadence transition (1500 -> 4000), effect remounts and triggers immediate poll (call 5)
            await vi.advanceTimersByTimeAsync(1_500)
            expect(callCount).toBe(5)

            // Now cadence is 4000ms. Advancing 3000ms must NOT trigger another poll yet
            await vi.advanceTimersByTimeAsync(3_000)
            expect(callCount).toBe(5)

            // Advancing 1000ms more (4000ms total since t = 4500) triggers call 6
            await vi.advanceTimersByTimeAsync(1_000)
            expect(callCount).toBe(6)

            cleanup()
        } finally {
            vi.useRealTimers()
        }
    })

    it('demonstrates that depending on status object identity causes runaway recursion at t=0', async () => {
        vi.useFakeTimers()
        try {
            let callCount = 0
            const fakeGetStatus = vi.fn(async () => {
                callCount++
                return telemetryStatus({ containerState: 'running' })
            })

            let cleanup: () => void = () => {}
            // Buggy effect simulation: re-triggers whenever a new status object reference is received
            const buggyMountOrUpdate = () => {
                cleanup()
                cleanup = scheduleTelemetryPolling(
                    fakeGetStatus,
                    _nextStatus => {
                        // In the buggy implementation, useEffect depended on [status].
                        // Each new status object deserialization re-triggered the effect immediately.
                        // We cap at 20 iterations to avoid infinite OOM in test environment.
                        if (callCount < 20) {
                            buggyMountOrUpdate()
                        }
                    },
                    undefined,
                    4000
                )
            }

            buggyMountOrUpdate()
            // At t = 0 without advancing any timers, the buggy loop fires 20 times in 0ms
            await vi.advanceTimersByTimeAsync(0)
            expect(callCount).toBe(20)

            cleanup()
        } finally {
            vi.useRealTimers()
        }
    })
})
