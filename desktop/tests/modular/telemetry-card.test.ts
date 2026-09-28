// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { TelemetryStatus } from '@/shared/types/telemetry'
import {
    getObservabilityBadge,
    getPhoenixDashboardUrl,
    isDockerUnavailable,
    isPhoenixRunning
} from '@/shared/utils/telemetry'
import getErrorString from '@/shared/utils/get-error-string'

const mocks = vi.hoisted(() => ({
    supervisor: {
        hasProcess: vi.fn(() => true),
        callProcess: vi.fn(),
        sendProcess: vi.fn(),
        reportError: vi.fn()
    },
    openExternal: vi.fn().mockResolvedValue(undefined),
    windowOpen: vi.fn()
}))

vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => mocks.supervisor
}))
vi.mock('@/electron/service-bridge/modular-state', () => ({
    getModularBridgeState: () => ({
        getSelfId: () => 'local-node'
    }),
    isProxyEngine: () => false,
    isUpstreamUnreachableError: () => false,
    parseServiceErrors: () => []
}))
vi.mock('@/electron/model-hub', () => ({ getEngineHubModels: vi.fn() }))

import { handleServiceBridgeInvoke } from '@/electron/service-bridge/empty-handlers'

describe('ObservabilityCard helpers and state management', () => {
    beforeEach(() => {
        vi.clearAllMocks()
    })

    describe('getObservabilityBadge', () => {
        it('returns Active ● (green) when collector is reachable', () => {
            const status: TelemetryStatus = {
                enabled: true,
                endpoint: 'localhost:4317',
                uiUrl: 'http://localhost:6006',
                recordPayloads: false,
                containerState: 'running',
                collectorReachable: true
            }
            const badge = getObservabilityBadge(status)
            expect(badge.label).toBe('Active ●')
            expect(badge.color).toBe('green')
        })

        it('returns Starting… (yellow) when container is running but collector not reachable', () => {
            const status: TelemetryStatus = {
                enabled: true,
                endpoint: 'localhost:4317',
                uiUrl: 'http://localhost:6006',
                recordPayloads: false,
                containerState: 'running',
                collectorReachable: false
            }
            const badge = getObservabilityBadge(status)
            expect(badge.label).toBe('Starting…')
            expect(badge.color).toBe('yellow')
        })

        it('returns Stopped (gray) when container is stopped', () => {
            const status: TelemetryStatus = {
                enabled: true,
                endpoint: 'localhost:4317',
                uiUrl: 'http://localhost:6006',
                recordPayloads: false,
                containerState: 'stopped',
                collectorReachable: false
            }
            const badge = getObservabilityBadge(status)
            expect(badge.label).toBe('Stopped')
            expect(badge.color).toBe('gray')
        })

        it('returns Stopped (gray) when status is null', () => {
            const badge = getObservabilityBadge(null)
            expect(badge.label).toBe('Stopped')
            expect(badge.color).toBe('gray')
        })

        it('returns Stopped (gray) when container is docker-unavailable', () => {
            const status: TelemetryStatus = {
                enabled: false,
                endpoint: 'localhost:4317',
                uiUrl: 'http://localhost:6006',
                recordPayloads: false,
                containerState: 'docker-unavailable',
                collectorReachable: false
            }
            const badge = getObservabilityBadge(status)
            expect(badge.label).toBe('Stopped')
            expect(badge.color).toBe('gray')
            expect(isDockerUnavailable(status)).toBe(true)
        })
    })

    describe('isPhoenixRunning', () => {
        it('returns true when collectorReachable is true', () => {
            const status: TelemetryStatus = {
                enabled: true,
                endpoint: 'localhost:4317',
                uiUrl: 'http://localhost:6006',
                recordPayloads: false,
                containerState: 'stopped',
                collectorReachable: true
            }
            expect(isPhoenixRunning(status)).toBe(true)
        })

        it('returns true when containerState is running', () => {
            const status: TelemetryStatus = {
                enabled: true,
                endpoint: 'localhost:4317',
                uiUrl: 'http://localhost:6006',
                recordPayloads: false,
                containerState: 'running',
                collectorReachable: false
            }
            expect(isPhoenixRunning(status)).toBe(true)
        })

        it('returns false when stopped and unreachable', () => {
            const status: TelemetryStatus = {
                enabled: true,
                endpoint: 'localhost:4317',
                uiUrl: 'http://localhost:6006',
                recordPayloads: false,
                containerState: 'stopped',
                collectorReachable: false
            }
            expect(isPhoenixRunning(status)).toBe(false)
        })

        it('returns false for null status', () => {
            expect(isPhoenixRunning(null)).toBe(false)
        })
    })

    describe('getPhoenixDashboardUrl', () => {
        it('returns the configured uiUrl when present', () => {
            const status: TelemetryStatus = {
                enabled: true,
                endpoint: 'localhost:4317',
                uiUrl: 'http://127.0.0.1:6006',
                recordPayloads: false,
                containerState: 'running',
                collectorReachable: true
            }
            expect(getPhoenixDashboardUrl(status)).toBe('http://127.0.0.1:6006')
        })

        it('defaults to http://localhost:6006 when uiUrl is empty or null', () => {
            expect(getPhoenixDashboardUrl(null)).toBe('http://localhost:6006')
            const emptyUrlStatus: TelemetryStatus = {
                enabled: true,
                endpoint: 'localhost:4317',
                uiUrl: '',
                recordPayloads: false,
                containerState: 'running',
                collectorReachable: true
            }
            expect(getPhoenixDashboardUrl(emptyUrlStatus)).toBe('http://localhost:6006')
        })
    })

    describe('telemetry lifecycle and state flows', () => {
        interface TelemetryControllerState {
            status: TelemetryStatus | null
            error: string | null
            loading: 'start' | 'stop' | null
        }

        class TelemetryController {
            public state: TelemetryControllerState = {
                status: null,
                error: null,
                loading: null
            }

            constructor(
                private readonly api: {
                    getStatus: () => Promise<TelemetryStatus>
                    start: () => Promise<TelemetryStatus>
                    stop: () => Promise<TelemetryStatus>
                    setEnabled: (val: boolean) => Promise<{ ok: boolean }>
                    setRecordPayloads: (val: boolean) => Promise<{ ok: boolean }>
                }
            ) {}

            async fetchStatus(): Promise<void> {
                try {
                    const s = await this.api.getStatus()
                    this.state.status = s
                } catch {
                    // ignore polling errors
                }
            }

            async start(): Promise<void> {
                this.state.loading = 'start'
                this.state.error = null
                try {
                    const s = await this.api.start()
                    this.state.status = s
                } catch (err) {
                    this.state.error = getErrorString(err)
                } finally {
                    this.state.loading = null
                }
            }

            async stop(): Promise<void> {
                this.state.loading = 'stop'
                this.state.error = null
                try {
                    const s = await this.api.stop()
                    this.state.status = s
                } catch (err) {
                    this.state.error = getErrorString(err)
                } finally {
                    this.state.loading = null
                }
            }

            async toggleEnabled(val: boolean): Promise<void> {
                if (this.state.status) {
                    this.state.status = { ...this.state.status, enabled: val }
                }
                try {
                    await this.api.setEnabled(val)
                } catch (err) {
                    this.state.error = getErrorString(err)
                    await this.fetchStatus()
                }
            }

            async toggleRecordPayloads(val: boolean): Promise<void> {
                if (this.state.status) {
                    this.state.status = { ...this.state.status, recordPayloads: val }
                }
                try {
                    await this.api.setRecordPayloads(val)
                } catch (err) {
                    this.state.error = getErrorString(err)
                    await this.fetchStatus()
                }
            }
        }

        it('fetches initial status successfully', async () => {
            const initialStatus: TelemetryStatus = {
                enabled: true,
                endpoint: 'localhost:4317',
                uiUrl: 'http://localhost:6006',
                recordPayloads: false,
                containerState: 'stopped',
                collectorReachable: false
            }
            const api = {
                getStatus: vi.fn().mockResolvedValue(initialStatus),
                start: vi.fn(),
                stop: vi.fn(),
                setEnabled: vi.fn(),
                setRecordPayloads: vi.fn()
            }

            const controller = new TelemetryController(api)
            await controller.fetchStatus()
            expect(controller.state.status).toEqual(initialStatus)
            expect(controller.state.error).toBeNull()
            expect(controller.state.loading).toBeNull()
        })

        it('manages starting lifecycle and updates state', async () => {
            const startedStatus: TelemetryStatus = {
                enabled: true,
                endpoint: 'localhost:4317',
                uiUrl: 'http://localhost:6006',
                recordPayloads: false,
                containerState: 'running',
                collectorReachable: true
            }
            const api = {
                getStatus: vi.fn(),
                start: vi.fn().mockResolvedValue(startedStatus),
                stop: vi.fn(),
                setEnabled: vi.fn(),
                setRecordPayloads: vi.fn()
            }

            const controller = new TelemetryController(api)
            const startPromise = controller.start()
            expect(controller.state.loading).toBe('start')
            await startPromise
            expect(controller.state.loading).toBeNull()
            expect(controller.state.status).toEqual(startedStatus)
            expect(controller.state.error).toBeNull()
        })

        it('captures errors when start fails and resets loading state', async () => {
            const api = {
                getStatus: vi.fn(),
                start: vi.fn().mockRejectedValue(new Error('Docker daemon not running')),
                stop: vi.fn(),
                setEnabled: vi.fn(),
                setRecordPayloads: vi.fn()
            }

            const controller = new TelemetryController(api)
            await controller.start()
            expect(controller.state.loading).toBeNull()
            expect(controller.state.error).toBe('Docker daemon not running')
        })

        it('manages stopping lifecycle and updates state', async () => {
            const stoppedStatus: TelemetryStatus = {
                enabled: true,
                endpoint: 'localhost:4317',
                uiUrl: 'http://localhost:6006',
                recordPayloads: false,
                containerState: 'stopped',
                collectorReachable: false
            }
            const api = {
                getStatus: vi.fn(),
                start: vi.fn(),
                stop: vi.fn().mockResolvedValue(stoppedStatus),
                setEnabled: vi.fn(),
                setRecordPayloads: vi.fn()
            }

            const controller = new TelemetryController(api)
            const stopPromise = controller.stop()
            expect(controller.state.loading).toBe('stop')
            await stopPromise
            expect(controller.state.loading).toBeNull()
            expect(controller.state.status).toEqual(stoppedStatus)
            expect(controller.state.error).toBeNull()
        })

        it('captures errors when stop fails and resets loading state', async () => {
            const api = {
                getStatus: vi.fn(),
                start: vi.fn(),
                stop: vi.fn().mockRejectedValue(new Error('Container not found')),
                setEnabled: vi.fn(),
                setRecordPayloads: vi.fn()
            }

            const controller = new TelemetryController(api)
            await controller.stop()
            expect(controller.state.loading).toBeNull()
            expect(controller.state.error).toBe('Container not found')
        })

        it('toggles tracing enabled successfully', async () => {
            const currentStatus: TelemetryStatus = {
                enabled: true,
                endpoint: 'localhost:4317',
                uiUrl: 'http://localhost:6006',
                recordPayloads: false,
                containerState: 'running',
                collectorReachable: true
            }
            const api = {
                getStatus: vi.fn(),
                start: vi.fn(),
                stop: vi.fn(),
                setEnabled: vi.fn().mockResolvedValue({ ok: true }),
                setRecordPayloads: vi.fn()
            }

            const controller = new TelemetryController(api)
            controller.state.status = currentStatus
            await controller.toggleEnabled(false)
            expect(api.setEnabled).toHaveBeenCalledWith(false)
            expect(controller.state.status?.enabled).toBe(false)
            expect(controller.state.error).toBeNull()
        })

        it('reverts and refetches when toggling tracing enabled fails', async () => {
            const serverStatus: TelemetryStatus = {
                enabled: true,
                endpoint: 'localhost:4317',
                uiUrl: 'http://localhost:6006',
                recordPayloads: false,
                containerState: 'running',
                collectorReachable: true
            }
            const api = {
                getStatus: vi.fn().mockResolvedValue(serverStatus),
                start: vi.fn(),
                stop: vi.fn(),
                setEnabled: vi.fn().mockRejectedValue(new Error('Failed to persist setting')),
                setRecordPayloads: vi.fn()
            }

            const controller = new TelemetryController(api)
            controller.state.status = { ...serverStatus }
            await controller.toggleEnabled(false)
            expect(api.setEnabled).toHaveBeenCalledWith(false)
            expect(controller.state.error).toBe('Failed to persist setting')
            expect(controller.state.status?.enabled).toBe(true)
        })

        it('toggles record payloads successfully', async () => {
            const currentStatus: TelemetryStatus = {
                enabled: true,
                endpoint: 'localhost:4317',
                uiUrl: 'http://localhost:6006',
                recordPayloads: false,
                containerState: 'running',
                collectorReachable: true
            }
            const api = {
                getStatus: vi.fn(),
                start: vi.fn(),
                stop: vi.fn(),
                setEnabled: vi.fn(),
                setRecordPayloads: vi.fn().mockResolvedValue({ ok: true })
            }

            const controller = new TelemetryController(api)
            controller.state.status = currentStatus
            await controller.toggleRecordPayloads(true)
            expect(api.setRecordPayloads).toHaveBeenCalledWith(true)
            expect(controller.state.status?.recordPayloads).toBe(true)
            expect(controller.state.error).toBeNull()
        })
    })

    describe('Service Bridge empty-handlers invocation for telemetry', () => {
        it('handles telemetry:get-status via supervisor broker call', async () => {
            mocks.supervisor.callProcess.mockResolvedValueOnce({
                enabled: true,
                endpoint: '127.0.0.1:4317',
                uiUrl: 'http://localhost:6006',
                recordPayloads: true,
                containerState: 'running',
                collectorReachable: true
            })

            const status = await handleServiceBridgeInvoke('telemetry:get-status', undefined)
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'telemetry/get-status'
            )
            expect(status).toEqual({
                enabled: true,
                endpoint: '127.0.0.1:4317',
                uiUrl: 'http://localhost:6006',
                recordPayloads: true,
                containerState: 'running',
                collectorReachable: true
            })
        })

        it('handles telemetry:start via supervisor broker call', async () => {
            mocks.supervisor.callProcess.mockResolvedValueOnce({
                enabled: true,
                endpoint: 'localhost:4317',
                uiUrl: 'http://localhost:6006',
                recordPayloads: false,
                containerState: 'running',
                collectorReachable: true
            })

            const status = await handleServiceBridgeInvoke('telemetry:start', undefined)
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'telemetry/start',
                undefined,
                15000
            )
            expect(status.containerState).toBe('running')
            expect(status.collectorReachable).toBe(true)
        })

        it('handles telemetry:stop via supervisor broker call', async () => {
            mocks.supervisor.callProcess.mockResolvedValueOnce({
                enabled: true,
                endpoint: 'localhost:4317',
                uiUrl: 'http://localhost:6006',
                recordPayloads: false,
                containerState: 'stopped',
                collectorReachable: false
            })

            const status = await handleServiceBridgeInvoke('telemetry:stop', undefined)
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'telemetry/stop',
                undefined,
                15000
            )
            expect(status.containerState).toBe('stopped')
            expect(status.collectorReachable).toBe(false)
        })

        it('handles telemetry:set-enabled via supervisor broker call', async () => {
            mocks.supervisor.callProcess.mockResolvedValueOnce({ ok: true })

            const res = await handleServiceBridgeInvoke('telemetry:set-enabled', {
                value: true
            })
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'settings/set-telemetry-enabled',
                { value: true }
            )
            expect(res).toEqual({ ok: true })
        })

        it('handles telemetry:set-record-payloads via supervisor broker call', async () => {
            mocks.supervisor.callProcess.mockResolvedValueOnce({ ok: true })

            const res = await handleServiceBridgeInvoke('telemetry:set-record-payloads', {
                value: true
            })
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'settings/set-telemetry-record-payloads',
                { value: true }
            )
            expect(res).toEqual({ ok: true })
        })
    })
})
