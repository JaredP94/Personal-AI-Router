// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { NodeItemMetrics } from '@/shared/types/metrics'
import type { JsonObject, JsonValue } from '@/electron/service-bridge/json-rpc-subprocess'
import { subscribePush } from '@/electron/service-bridge/push-bus'
import { useMetricsStore } from '@/ui/stores/metrics.store'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'

const nodeId = 'sensors-test'
const updates: NodeItemMetrics[] = []
const upserts: string[] = []
let unsubscribe = () => {}

function discover(name = 'fixture'): void {
    getModularBridgeState().handleNotification({
        source: 'broker',
        method: 'discovery:nodes-changed',
        params: { nodes: [{ hostUuid: nodeId, name, ipAddress: '192.0.2.1', port: 14318 }] }
    })
}

function merge(sensors?: JsonValue, telemetry: JsonObject = { GPUs: [] }): void {
    getModularBridgeState().mergeNodeInfoResponse(
        nodeId,
        sensors === undefined ? telemetry : { ...telemetry, sensors }
    )
}

function current() {
    return updates[updates.length - 1].current
}

function storedSensors() {
    return useMetricsStore.getState().nodeMetrics.get(nodeId)?.sensors
}

describe('node-info sensors through metrics updates and renderer state', () => {
    beforeEach(() => {
        vi.useFakeTimers()
        vi.setSystemTime(100_000)
        discover()
        merge()
        updates.length = 0
        upserts.length = 0
        unsubscribe = subscribePush(event => {
            if (event.channel === 'metrics:update') updates.push(event.payload)
            if (event.channel === 'nodes:upsert') upserts.push(event.payload.id)
        })
        vi.stubGlobal('window', {
            pairApi: {
                metrics: {
                    onRoutingUpdate: () => () => {},
                    getRouting: async () => [],
                    onUpdate: (callback: (metrics: NodeItemMetrics) => void) =>
                        subscribePush(event => {
                            if (event.channel === 'metrics:update') callback(event.payload)
                        }),
                    getNodeMetrics: async () => ({ nodes: [] }),
                    onNodeMetrics: () => () => {}
                },
                nodes: { onRemove: () => () => {} }
            }
        })
        useMetricsStore.getState().clearAll()
        useMetricsStore.getState().initialize()
    })

    afterEach(() => {
        unsubscribe()
        useMetricsStore.getState().cleanup()
        vi.unstubAllGlobals()
        vi.useRealTimers()
    })

    it('parses absent sensors as null', () => {
        merge()
        expect(current().sensors).toBeNull()
    })

    it('parses a full sensors object', () => {
        merge({
            macmon: 'available',
            thermalPressure: 'nominal',
            memoryPressure: 'normal',
            swapTotalBytes: 1073741824,
            swapUsedBytes: 6291456,
            cpuTempC: 36,
            gpuTempC: 35.3,
            fans: [
                { name: 'fan0', rpm: 0, maxRpm: 5349 },
                { name: 'fan1', rpm: 0, maxRpm: 5777 }
            ],
            power: { systemW: 3.86, cpuW: 0.18, gpuW: 0.12, aneW: 0 },
            msSince: 412
        })
        expect(current().sensors).toEqual({
            macmon: 'available',
            thermalPressure: 'nominal',
            memoryPressure: 'normal',
            swapTotalBytes: 1073741824,
            swapUsedBytes: 6291456,
            cpuTempC: 36,
            gpuTempC: 35.3,
            fans: [
                { name: 'fan0', rpm: 0, maxRpm: 5349 },
                { name: 'fan1', rpm: 0, maxRpm: 5777 }
            ],
            power: { systemW: 3.86, cpuW: 0.18, gpuW: 0.12, aneW: 0 }
        })
    })

    it('keeps fans [] distinct from absent fans', () => {
        merge({ macmon: 'available', fans: [] })
        expect(current().sensors?.fans).toEqual([])
        merge({ macmon: 'available' })
        expect(current().sensors?.fans).toBeNull()
    })

    it('treats unknown enum strings as null', () => {
        merge({ macmon: 'maybe', thermalPressure: 'boiling', memoryPressure: 'full' })
        expect(current().sensors).toMatchObject({
            macmon: null,
            thermalPressure: null,
            memoryPressure: null
        })
    })

    it.each(['available', 'missing', 'failed'])('accepts macmon status %s', macmon => {
        merge({ macmon })
        expect(current().sensors?.macmon).toBe(macmon)
    })

    it.each(['nominal', 'moderate', 'heavy', 'trapping', 'sleeping'])(
        'accepts thermal pressure %s',
        thermalPressure => {
            merge({ thermalPressure })
            expect(current().sensors?.thermalPressure).toBe(thermalPressure)
        }
    )

    it.each(['normal', 'warning', 'critical'])('accepts memory pressure %s', memoryPressure => {
        merge({ memoryPressure })
        expect(current().sensors?.memoryPressure).toBe(memoryPressure)
    })

    it.each<{ sensors: JsonValue }>([
        { sensors: null },
        { sensors: [] },
        { sensors: 'invalid' },
        { sensors: 1 }
    ])('treats malformed sensors $sensors as null', ({ sensors }) => {
        merge(sensors)
        expect(current().sensors).toBeNull()
    })

    it('keeps unmeasured fields null instead of inventing zero readings', () => {
        merge({ macmon: 1, thermalPressure: false, memoryPressure: [], cpuTempC: '36' })
        expect(current().sensors).toEqual({
            macmon: null,
            thermalPressure: null,
            memoryPressure: null,
            swapTotalBytes: null,
            swapUsedBytes: null,
            cpuTempC: null,
            gpuTempC: null,
            fans: null,
            power: null
        })
    })

    it('keeps measured zero values and drops incomplete or malformed fans', () => {
        merge({
            swapTotalBytes: 0,
            swapUsedBytes: 0,
            cpuTempC: 0,
            gpuTempC: 0,
            fans: [
                { name: 'fan0', rpm: 0, maxRpm: 5349 },
                { rpm: 10, maxRpm: 20 },
                { name: 'missing rpm', maxRpm: 20 },
                { name: 'missing maxRpm', rpm: 10 },
                { name: 'string rpm', rpm: '10', maxRpm: 20 },
                { name: 1, rpm: 10, maxRpm: 20 },
                null
            ]
        })
        expect(current().sensors).toMatchObject({
            swapTotalBytes: 0,
            swapUsedBytes: 0,
            cpuTempC: 0,
            gpuTempC: 0,
            fans: [{ name: 'fan0', rpm: 0, maxRpm: 5349 }]
        })
        merge({ fans: 'invalid' })
        expect(current().sensors?.fans).toBeNull()
    })

    it.each<{ power: JsonValue }>([
        { power: {} },
        { power: { systemW: '3.86' } },
        { power: null },
        { power: [] }
    ])('treats power without numeric readings $power as null', ({ power }) => {
        merge({ power })
        expect(current().sensors?.power).toBeNull()
    })

    it.each(['systemW', 'cpuW', 'gpuW', 'aneW'])(
        'keeps a zero-only power reading for %s with other fields null',
        field => {
            merge({ power: { [field]: 0 } })
            expect(current().sensors?.power).toEqual({
                systemW: null,
                cpuW: null,
                gpuW: null,
                aneW: null,
                [field]: 0
            })
        }
    )

    it('emits a metrics update when only sensors change', () => {
        merge({ cpuTempC: 40 })
        vi.advanceTimersByTime(1000)
        merge({ cpuTempC: 41 })
        expect(updates).toHaveLength(2)
        expect(current().sensors?.cpuTempC).toBe(41)
    })

    it('does not upsert the node when only sensors change', () => {
        merge({ cpuTempC: 40 })
        upserts.length = 0
        vi.advanceTimersByTime(1000)
        merge({ cpuTempC: 41 })
        expect(upserts).toEqual([])
        expect(current().sensors?.cpuTempC).toBe(41)
    })

    it('stores latest sensors in the metrics store and clears them when absent', () => {
        merge({ cpuTempC: 40 })
        expect(storedSensors()?.cpuTempC).toBe(40)
        vi.advanceTimersByTime(1000)
        merge({ cpuTempC: 41 })
        expect(storedSensors()?.cpuTempC).toBe(41)
        vi.advanceTimersByTime(1000)
        merge()
        expect(storedSensors()).toBeNull()
    })

    it('ignores repeated timestamps without replacing the accepted sensor reading', () => {
        merge({ cpuTempC: 40 })
        merge({ cpuTempC: 41 })
        expect(storedSensors()?.cpuTempC).toBe(40)
        expect(useMetricsStore.getState().nodeMetrics.get(nodeId)?.cpuUtilization).toHaveLength(1)
    })

    it('retains the latest sensor-only reading for later broker and proxy upserts', () => {
        merge({ cpuTempC: 40 })
        vi.advanceTimersByTime(1000)
        merge({ cpuTempC: 41 })
        vi.advanceTimersByTime(1000)
        discover('renamed')
        expect(upserts).toContain(nodeId)
        expect(current().sensors?.cpuTempC).toBe(41)
        vi.advanceTimersByTime(1000)
        getModularBridgeState().handleNotification({
            source: 'proxy',
            method: 'node/discovered',
            params: { id: nodeId, host: 'fixture', ip: '192.0.2.1', port: 11434 }
        })
        expect(upserts).toEqual([nodeId, nodeId])
        expect(current().timestamp).toBe(103_000)
        expect(current().sensors?.cpuTempC).toBe(41)
    })

    it('clears sensors with other telemetry when discovery removes the node-info source', () => {
        getModularBridgeState().handleNotification({
            source: 'proxy',
            method: 'node/discovered',
            params: { id: nodeId, host: 'fixture', ip: '192.0.2.1', port: 11434 }
        })
        merge({ cpuTempC: 40 })
        vi.advanceTimersByTime(1000)
        getModularBridgeState().handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: { nodes: [] }
        })
        expect(current().sensors).toBeNull()
        expect(storedSensors()).toBeNull()
    })

    it('includes current sensors when other telemetry changes and upserts', () => {
        merge({ cpuTempC: 40 })
        vi.advanceTimersByTime(1000)
        merge({ cpuTempC: 41 })
        vi.advanceTimersByTime(1000)
        merge(
            { cpuTempC: 42 },
            { GPUs: [], cpu: { name: 'CPU', cores: 16, utilization_percent: 2 } }
        )
        expect(upserts).toContain(nodeId)
        expect(current().sensors?.cpuTempC).toBe(42)
        expect(storedSensors()?.cpuTempC).toBe(42)
    })
})
