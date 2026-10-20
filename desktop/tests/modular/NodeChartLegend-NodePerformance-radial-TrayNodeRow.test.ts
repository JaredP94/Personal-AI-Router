// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// @vitest-environment jsdom

import { act, createElement, type ReactNode } from 'react'
import { createRoot } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { GpuInfo as HardwareGpuInfo, GpuMemoryKind } from '@/shared/types/hardware'
import type { NodeItem } from '@/shared/types/nodes'
import type { GpuInfo } from '@/ui/types/node-hardware'
import { useMetricsStore, type NodeMetricsHistory } from '@/ui/stores/metrics.store'
import NodeChartLegend from '@/ui/components/NodeList/NodeChartLegend'
import NodePerformance from '@/ui/components/NodeList/NodePerformance'
import TrayNodeRow from '@/ui/components/TrayApp/TrayNodeRow'
import SvgLineChart from '@/ui/components/SvgLineChart'
import GpuRadialChartSvg from '@/ui/components/GpuRadialChartSvg'
import { buildRadialChartMetrics } from '@/ui/utils/build-radial-chart-metrics'

const GiB = 1024 ** 3
let container: HTMLDivElement
let root: ReturnType<typeof createRoot>

function gpu(memoryKind: GpuMemoryKind, overrides: Partial<HardwareGpuInfo> = {}): HardwareGpuInfo {
    return {
        id: 'gpu-0',
        name: 'Fixture GPU',
        memoryKind,
        vramTotal: 8 * GiB,
        sharedTotal: null,
        ...overrides
    }
}

function node(gpus: HardwareGpuInfo[]): NodeItem {
    return {
        id: 'fixture',
        name: 'Fixture node',
        ipAddress: '192.0.2.1',
        port: 14318,
        allIpAddresses: [],
        status: 'active',
        os: 'Linux',
        topology: {
            gpus,
            cpu: { model: 'Fixture CPU', cores: 4, threads: 4 },
            ram: 16 * GiB,
            storage: []
        }
    }
}

function history(
    utilization: (number | null)[] = [null],
    memory: (number | null)[] = [25],
    sharedUsed: number | null = null
): NodeMetricsHistory {
    const points = (values: (number | null)[]) =>
        values.map((value, timestamp) => ({ timestamp, value }))
    return {
        gpuUtilization: [{ id: 'gpu-0', data: points(utilization) }],
        gpuVramUsage: [{ id: 'gpu-0', data: points(memory), sharedUsed: points([sharedUsed]) }],
        cpuUtilization: points([0]),
        memoryUsage: points([0])
    }
}

function legendGpu(hardware: HardwareGpuInfo, overrides: Partial<GpuInfo> = {}): GpuInfo {
    return {
        ...hardware,
        sharedUsed: null,
        vramUsage: 25,
        usage: null,
        usageColor: '#00ff00',
        vramColor: '#0000ff',
        ...overrides
    }
}

async function render(content: ReactNode) {
    await act(async () => root.render(content))
}

function text() {
    const walker = document.createTreeWalker(container, NodeFilter.SHOW_TEXT)
    const parts: string[] = []
    while (walker.nextNode()) parts.push(walker.currentNode.textContent ?? '')
    return parts.join(' ').replace(/\s+/g, ' ').trim()
}

beforeEach(() => {
    vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true)
    vi.stubGlobal(
        'ResizeObserver',
        class {
            observe = vi.fn()
            disconnect = vi.fn()
        }
    )
    vi.stubGlobal('requestAnimationFrame', () => 1)
    vi.stubGlobal('cancelAnimationFrame', () => {})
    container = document.createElement('div')
    document.body.append(container)
    root = createRoot(container)
    useMetricsStore.setState({ nodeMetrics: new Map([['fixture', history()]]) })
})

afterEach(async () => {
    await act(async () => root.unmount())
    container.remove()
    useMetricsStore.getState().clearAll()
    vi.unstubAllGlobals()
})

const cases: { kind: GpuMemoryKind; label: string; detail: string }[] = [
    { kind: 'dedicated', label: 'VRAM', detail: '2 GB / 8 GB' },
    {
        kind: 'unified',
        label: 'Unified memory',
        detail: '2 GB used of 8 GB unified memory'
    },
    { kind: 'shared', label: 'Shared memory', detail: '2 GB / 8 GB' },
    { kind: 'unavailable', label: 'Memory unavailable', detail: '' }
]

for (const { kind, label, detail } of cases) {
    describe(`${kind} memory presentation`, () => {
        it('NodeChartLegend uses the exact label and detail, with unknown utilization', async () => {
            await render(createElement(NodeChartLegend, { gpuInfo: [legendGpu(gpu(kind))] }))
            expect(text()).toContain(label)
            expect(text()).toContain('Usage —')
            if (detail) expect(text()).toContain(detail)
            else expect(text()).not.toMatch(/GB|\//)
        })
        it('NodePerformance labels the legend and dataset by kind', async () => {
            await render(createElement(NodePerformance, { node: node([gpu(kind)]) }))
            expect(text()).toContain(label)
            expect(text()).toContain('Usage —')
            if (detail) expect(text()).toContain(detail)
            else expect(text()).not.toContain('8 GB')
        })
        it('TrayNodeRow uses the same labels and figures', async () => {
            await render(
                createElement(TrayNodeRow, { node: node([gpu(kind)]), activeWorkloads: [] })
            )
            expect(text()).toContain(label)
            if (detail) expect(text()).toContain(detail)
            else expect(text()).not.toMatch(/\d+ (?:GB|MB|KB|B)/)
        })
    })
}

it.each([null, 0, GiB])(
    'shows Windows shared usage %s in bytes independently of dedicated memory',
    async sharedUsed => {
        const hardware = gpu('dedicated', { sharedTotal: 4 * GiB })
        useMetricsStore.setState({
            nodeMetrics: new Map([['fixture', history([0], [25], sharedUsed)]])
        })
        const expected = `Shared memory ${sharedUsed === null ? '—' : sharedUsed === 0 ? '0 B' : '1 GB'} / 4 GB`
        for (const component of [
            createElement(NodeChartLegend, {
                gpuInfo: [legendGpu(hardware, { usage: 0, sharedUsed })]
            }),
            createElement(NodePerformance, { node: node([hardware]) }),
            createElement(TrayNodeRow, { node: node([hardware]), activeWorkloads: [] })
        ]) {
            await render(component)
            expect(text()).toContain('VRAM 2 GB / 8 GB')
            expect(text()).toContain(expected)
            if (component.type !== TrayNodeRow) expect(text()).toContain('Usage 0%')
        }
    }
)

it('does not invent used bytes when percentage or capacity is unknown', async () => {
    for (const [capacity, percent] of [
        [8 * GiB, null],
        [null, 25],
        [null, null]
    ]) {
        const hardware = gpu('unified', { vramTotal: capacity })
        await act(async () =>
            useMetricsStore.setState({
                nodeMetrics: new Map([['fixture', history([null], [percent])]])
            })
        )
        for (const component of [
            createElement(NodePerformance, { node: node([hardware]) }),
            createElement(TrayNodeRow, { node: node([hardware]), activeWorkloads: [] })
        ]) {
            await render(component)
            expect(text()).toContain(`— used of ${capacity === null ? '—' : '8 GB'} unified memory`)
        }
    }
})

it('keeps two unified capacities separate rather than adding them', async () => {
    const gpus = [gpu('unified'), gpu('unified', { id: 'gpu-1', name: 'Second GPU' })]
    for (const component of [
        createElement(NodePerformance, { node: node(gpus) }),
        createElement(TrayNodeRow, { node: node(gpus), activeWorkloads: [] })
    ]) {
        await render(component)
        expect(text().match(/of 8 GB unified memory/g)).toHaveLength(2)
        expect(text()).not.toContain('16 GB unified memory')
    }
})

it.each(cases)('keeps measured idle memory distinct from unknown for $kind', async ({ kind }) => {
    const hardware = gpu(kind)
    await act(async () =>
        useMetricsStore.setState({ nodeMetrics: new Map([['fixture', history([0], [0])]]) })
    )
    for (const component of [
        createElement(NodeChartLegend, {
            gpuInfo: [legendGpu(hardware, { usage: 0, vramUsage: 0 })]
        }),
        createElement(NodePerformance, { node: node([hardware]) }),
        createElement(TrayNodeRow, { node: node([hardware]), activeWorkloads: [] })
    ]) {
        await render(component)
        if (kind === 'unavailable') expect(text()).toContain('Memory unavailable')
        else
            expect(text()).toContain(
                kind === 'unified' ? '0 B used of 8 GB unified memory' : '0 B / 8 GB'
            )
    }
})

it('radial builder keeps unknown rings in position and idle zero measured', () => {
    const topology = node([gpu('unified'), gpu('shared', { id: 'gpu-1' })]).topology
    const metrics = buildRadialChartMetrics(topology, history([0], [null]))
    expect(metrics.map(metric => metric.value)).toEqual([0, null, null, null])
})

it('radial never plots numeric memory for unavailable kind and preserves readiness and CPU fallback', () => {
    const topology = node([gpu('unavailable')]).topology
    expect(
        buildRadialChartMetrics(topology, history([0], [25])).map(metric => metric.value)
    ).toEqual([0, null])
    topology.inferenceHardwareIds = []
    expect(buildRadialChartMetrics(topology, history()).map(metric => metric.value)).toEqual([0, 0])
})

it('radial shows empty tracks and em dashes at null, including a known-to-null transition', async () => {
    await render(
        createElement(GpuRadialChartSvg, {
            metrics: [{ value: 60, color: '#00ff00', shown: true }]
        })
    )
    expect(container.querySelector('path')).not.toBeNull()
    const metrics = buildRadialChartMetrics(
        node([gpu('unified')]).topology,
        history([null], [null])
    )
    await render(createElement(GpuRadialChartSvg, { metrics }))
    expect(container.querySelectorAll('circle')).toHaveLength(2)
    expect(container.querySelector('path')).toBeNull()
    expect(text()).toContain('—')
    expect(container.textContent).not.toContain('0%')
})

it('radial recovers measured readings after a gap without blanking another adapter', async () => {
    const hardware = node([gpu('unified'), gpu('unified', { id: 'gpu-1' })])
    await render(
        createElement(GpuRadialChartSvg, {
            metrics: buildRadialChartMetrics(hardware.topology, history([null], [null]))
        })
    )
    expect(container.querySelector('path')).toBeNull()
    const measured = buildRadialChartMetrics(hardware.topology, history([50], [25]))
    await render(createElement(GpuRadialChartSvg, { metrics: measured }))
    expect(container.querySelectorAll('circle')).toHaveLength(4)
    expect(container.querySelector('path')).not.toBeNull()
    expect(text()).toContain('—')
})

it.each([
    { value: 50, y: '100' },
    { value: 0, y: '196' }
])('line chart visibly marks an isolated measured $value between gaps', async ({ value, y }) => {
    await render(
        createElement(SvgLineChart, {
            datasets: [
                {
                    key: 'gpu',
                    label: 'GPU',
                    color: '#00ff00',
                    data: [null, value, null].map((value, timestamp) => ({ timestamp, value }))
                }
            ]
        })
    )
    const markers = container.querySelectorAll('circle[fill="#00ff00"]')
    expect(markers).toHaveLength(1)
    expect(markers[0].getAttribute('cx')).toBe('150')
    expect(markers[0].getAttribute('cy')).toBe(y)
    expect(Number(markers[0].getAttribute('r'))).toBeGreaterThan(0)
    expect(container.querySelector('path[stroke]')?.getAttribute('d')).not.toContain('C ')
})

it('line chart recovers isolated and connected measurements after gaps without joining them', async () => {
    const dataset = {
        key: 'gpu',
        label: 'GPU',
        color: '#00ff00',
        data: [null, null, null].map((value, timestamp) => ({ timestamp, value }))
    }
    await render(createElement(SvgLineChart, { datasets: [dataset] }))
    expect(container.querySelector('path')).toBeNull()
    expect(container.querySelector('circle')).toBeNull()

    await render(
        createElement(SvgLineChart, {
            datasets: [
                {
                    ...dataset,
                    data: [null, 50, null, 0, 20, null].map((value, timestamp) => ({
                        timestamp,
                        value
                    }))
                }
            ]
        })
    )
    expect(container.querySelectorAll('circle[fill="#00ff00"]')).toHaveLength(1)
    const stroke = container.querySelector('path[stroke="#00ff00"]')?.getAttribute('d') ?? ''
    expect(stroke.match(/M /g)).toHaveLength(2)
    expect(stroke.match(/C /g)).toHaveLength(1)
    expect(stroke).toContain('M 62.4 100')
    expect(stroke).toContain('M 179.2 196')

    await render(createElement(SvgLineChart, { datasets: [dataset] }))
    expect(container.querySelector('path')).toBeNull()
    expect(container.querySelector('circle')).toBeNull()
})

it('line chart breaks both strokes and fills at null points, preserving x positions and real zero', async () => {
    const values = [null, 50, 40, null, 0, 20, null]
    await render(
        createElement(SvgLineChart, {
            datasets: [
                {
                    key: 'gpu',
                    label: 'GPU',
                    color: '#00ff00',
                    data: values.map((value, timestamp) => ({ timestamp, value }))
                }
            ]
        })
    )
    const stroke = container.querySelector('path[stroke="#00ff00"]')?.getAttribute('d') ?? ''
    expect(stroke.match(/M /g)).toHaveLength(2)
    expect(stroke).toContain('M 52.666666666666664 100')
    expect(stroke).toContain('M 198.66666666666666 196')
    const fill = container.querySelector('path[fill^="url"]')?.getAttribute('d') ?? ''
    expect(fill.match(/M /g)).toHaveLength(2)
    expect(fill.match(/ Z/g)).toHaveLength(2)
    await render(
        createElement(SvgLineChart, {
            datasets: [
                {
                    key: 'gpu',
                    label: 'GPU',
                    color: '#00ff00',
                    data: [{ timestamp: 0, value: null }]
                }
            ]
        })
    )
    expect(container.querySelector('path')).toBeNull()
})
