// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// @vitest-environment jsdom

import { act, createElement, type ReactNode } from 'react'
import { createRoot } from 'react-dom/client'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import type {
    MacmonInstallOutcome,
    MemoryPressure,
    NodeSensors,
    ThermalPressure
} from '@/shared/types/metrics'
import type { NodeItem } from '@/shared/types/nodes'
import NodeCardDetails from '@/ui/components/NodeList/NodeCardDetails'
import NodeSensorsPanel from '@/ui/components/NodeList/NodeSensors'
import NodeSensorBadges from '@/ui/components/NodeList/NodeSensorBadges'
import { memoryTone, thermalTone } from '@/ui/utils/sensor-levels'
import { useMetricsStore } from '@/ui/stores/metrics.store'
import { useDiscoveredNodesStore } from '@/ui/stores/discovered-nodes.store'
import { useConnectionStore } from '@/ui/stores/connection.store'

const GiB = 1024 ** 3
const node: NodeItem = {
    id: 'fixture',
    name: 'Fixture Mac',
    ipAddress: '192.0.2.1',
    port: 14318,
    allIpAddresses: [],
    status: 'active',
    os: 'MacOS',
    topology: {
        gpus: [],
        cpu: { model: 'Apple M5 Max', cores: 18, threads: 18 },
        ram: 128 * GiB,
        storage: []
    }
}

function readings(overrides: Partial<NodeSensors> = {}): NodeSensors {
    return {
        macmon: 'available',
        thermalPressure: 'nominal',
        memoryPressure: 'normal',
        swapUsedBytes: 2.1 * GiB,
        swapTotalBytes: 4 * GiB,
        cpuTempC: 36.04,
        gpuTempC: 35.26,
        fans: [
            { name: 'Left', rpm: 1234, maxRpm: 5349 },
            { name: 'Right', rpm: 1456, maxRpm: 5600 }
        ],
        power: { systemW: 4.2, cpuW: 1.7, gpuW: 0.1, aneW: 0 },
        ...overrides
    }
}

let container: HTMLDivElement
let root: ReturnType<typeof createRoot>
const installMacmon = vi.fn<(nodeId: string) => Promise<MacmonInstallOutcome>>()
const copyToClipboard = vi.fn<(value: string) => Promise<void>>()

async function render(content: ReactNode) {
    await act(async () => root.render(content))
}

function text(element: Node = container) {
    const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT)
    const parts: string[] = []
    while (walker.nextNode()) parts.push(walker.currentNode.textContent ?? '')
    return parts.join(' ').replace(/\s+/g, ' ').trim()
}

function tile(label: string) {
    const element = container.querySelector(`[aria-label="${label}"]`)
    if (!element) throw new Error(`Missing ${label} tile`)
    return element
}

function button(label: string) {
    const element = Array.from(container.querySelectorAll('button')).find(
        element => element.getAttribute('aria-label') === label || text(element) === label
    )
    if (!element) throw new Error(`Missing ${label} button`)
    return element
}

async function click(element: Element) {
    await act(async () => element.dispatchEvent(new MouseEvent('click', { bubbles: true })))
}

async function panel(sensors = readings(), isLocal = true) {
    await render(createElement(NodeSensorsPanel, { node, sensors, isLocal }))
}

function setMetrics(sensors: NodeSensors | null) {
    useMetricsStore.setState({
        nodeMetrics: new Map([
            [
                node.id,
                {
                    cpuUtilization: [{ timestamp: 1, value: 0 }],
                    memoryUsage: [{ timestamp: 1, value: 0 }],
                    gpuUtilization: [],
                    gpuVramUsage: [],
                    sensors
                }
            ]
        ])
    })
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
    installMacmon.mockReset().mockResolvedValue('failed')
    copyToClipboard.mockReset().mockResolvedValue(undefined)
    vi.stubGlobal('pairApi', { nodes: { installMacmon } })
    vi.stubGlobal('windowApi', { window: { copyToClipboard } })
    useDiscoveredNodesStore.setState({ nodes: [] })
    useConnectionStore.setState({ selfId: node.id })
    container = document.createElement('div')
    document.body.append(container)
    root = createRoot(container)
})

afterEach(async () => {
    await act(async () => root.unmount())
    container.remove()
    useMetricsStore.setState({ nodeMetrics: new Map(), nodeInferenceMetrics: new Map() })
    useDiscoveredNodesStore.setState({ nodes: [] })
    useConnectionStore.setState({ selfId: '' })
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
})

it('renders no sensors button without sensors', async () => {
    setMetrics(null)
    await render(createElement(NodeCardDetails, { node }))
    expect(container.querySelector('button[aria-label*="sensors"]')).toBeNull()
})

it.each([400, 700])('wires the sensors drawer and badges at card width %s', async width => {
    vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockReturnValue(
        new DOMRect(0, 0, width, 300)
    )
    setMetrics(readings({ thermalPressure: 'moderate', memoryPressure: 'warning' }))
    await render(createElement(NodeCardDetails, { node }))
    expect(text()).toContain('Throttling Memory pressure')
    const labels = Array.from(container.querySelectorAll('button')).map(element =>
        element.getAttribute('aria-label')
    )
    expect(labels.indexOf('Expand sensors')).toBe(labels.indexOf('Expand performance metrics') + 1)
    expect(labels.indexOf('Engine settings')).toBe(labels.indexOf('Expand sensors') + 1)
    await click(button('Expand sensors'))
    expect(button('Collapse sensors').getAttribute('aria-expanded')).toBe('true')
    expect(text(tile('Temperature'))).toContain('36 °C')
    await click(button('Expand performance metrics'))
    expect(button('Expand sensors').getAttribute('aria-expanded')).toBe('false')
    await click(button('Expand sensors'))
    expect(button('Expand performance metrics').getAttribute('aria-expanded')).toBe('false')
    await click(button('Collapse sensors'))
    expect(button('Expand sensors').getAttribute('aria-expanded')).toBe('false')
})

it('renders the four tiles with M5 Max values', async () => {
    await panel()
    expect(
        Array.from(container.querySelectorAll('section')).map(element =>
            element.getAttribute('aria-label')
        )
    ).toEqual(['Temperature', 'Fans', 'Power', 'Memory'])
    expect(text(tile('Temperature'))).toBe('Temperature 36 °C CPU 36 °C · GPU 35 °C')
    expect(text(tile('Fans'))).toBe('Fans 1,456 rpm of 5,349 rpm of 5,600 rpm')
    expect(text(tile('Power'))).toBe('Power 4 W CPU 2 · GPU 0 · ANE 0 W')
    expect(text(tile('Memory'))).toBe('Memory Normal swap 2.1 GB / 4 GB')
})

it('renders No fans for an empty fan list', async () => {
    await panel(readings({ fans: [] }))
    expect(text(tile('Fans'))).toBe('Fans No fans')
    expect(Array.from(tile('Fans').children).map(element => text(element))).toEqual([
        'Fans',
        'No fans'
    ])
})

it('keeps unknown values distinct from measured zero', async () => {
    await panel(
        readings({
            cpuTempC: null,
            gpuTempC: null,
            fans: null,
            power: null,
            memoryPressure: null,
            swapUsedBytes: null,
            swapTotalBytes: null
        })
    )
    expect(text(tile('Temperature'))).toBe('Temperature — CPU — · GPU —')
    expect(text(tile('Fans'))).toBe('Fans —')
    expect(text(tile('Power'))).toBe('Power — CPU — · GPU — · ANE — W')
    expect(text(tile('Memory'))).toBe('Memory — swap — / —')
    await panel(
        readings({
            cpuTempC: 0,
            gpuTempC: null,
            fans: [{ name: 'Left', rpm: 0, maxRpm: 5349 }],
            power: { systemW: 0, cpuW: null, gpuW: 0, aneW: null },
            swapUsedBytes: 0
        })
    )
    expect(text(tile('Temperature'))).toBe('Temperature 0 °C CPU 0 °C · GPU —')
    expect(text(tile('Fans'))).toContain('Fans 0 rpm')
    expect(text(tile('Power'))).toBe('Power 0 W CPU — · GPU 0 · ANE — W')
    expect(text(tile('Memory'))).toContain('swap 0 B / 4 GB')
})

it('uses the higher known temperature even when only GPU is measured', async () => {
    await panel(readings({ cpuTempC: null, gpuTempC: 41.8 }))
    expect(text(tile('Temperature'))).toBe('Temperature 42 °C CPU — · GPU 42 °C')
})

it('renders dashes and Install macmon when macmon is missing locally', async () => {
    await panel(readings({ macmon: 'missing' }))
    for (const label of ['Temperature', 'Fans', 'Power'])
        expect(text(tile(label))).toBe(`${label} —`)
    expect(text(tile('Memory'))).toContain('Normal swap 2.1 GB / 4 GB')
    expect(text()).toContain('Sensors unavailable — Install macmon')
    expect(container.textContent).toContain('Sensors unavailable — Install macmon')
    expect(button('Install macmon').disabled).toBe(false)
})

it('renders the copy command instead of Install for an untrusted peer', async () => {
    await panel(readings({ macmon: 'missing' }), false)
    expect(text()).toContain('Sensors unavailable — run brew install macmon on this node')
    expect(text()).not.toContain('Install macmon')
    await click(button('Copy command'))
    expect(copyToClipboard).toHaveBeenCalledWith('brew install macmon')
})

it('allows Install for a trusted peer matched by node id', async () => {
    useDiscoveredNodesStore.setState({
        nodes: [
            {
                id: node.id,
                name: 'Different display name',
                ipAddress: '192.0.2.1',
                port: 14318,
                lastSeen: 0,
                trusted: true,
                clustered: true
            }
        ]
    })
    await panel(readings({ macmon: 'missing' }), false)
    await click(button('Install macmon'))
    expect(installMacmon).toHaveBeenCalledWith(node.id)
    expect(button('Install macmon').disabled).toBe(false)
})

it('does not use another peer’s trusted flag for an untrusted node', async () => {
    useDiscoveredNodesStore.setState({
        nodes: [
            {
                id: 'another-peer',
                name: node.name,
                ipAddress: node.ipAddress,
                port: 14318,
                lastSeen: 0,
                trusted: true,
                clustered: true
            }
        ]
    })
    await panel(readings({ macmon: 'missing' }), false)
    expect(text()).toContain('Sensors unavailable — run brew install macmon on this node')
    expect(text()).not.toContain('Install macmon')
})

it('shows the Homebrew copy command after a homebrew-missing outcome', async () => {
    installMacmon.mockResolvedValue('homebrew-missing')
    await panel(readings({ macmon: 'missing' }))
    await click(button('Install macmon'))
    expect(text()).toContain('Install Homebrew, then run brew install macmon')
    expect(text()).not.toContain('Sensors unavailable')
    await click(button('Copy command'))
    expect(copyToClipboard).toHaveBeenCalledWith('brew install macmon')
})

it('disables Install and shows the existing spinner while installing', async () => {
    let finish: (outcome: MacmonInstallOutcome) => void = () => {}
    installMacmon.mockImplementation(
        () =>
            new Promise(resolve => {
                finish = resolve
            })
    )
    await panel(readings({ macmon: 'missing' }))
    await click(button('Install macmon'))
    expect(button('Install macmon').disabled).toBe(true)
    expect(button('Install macmon').querySelector('.spinner-element[role="status"]')).not.toBeNull()
    await act(async () => finish('failed'))
    expect(button('Install macmon').disabled).toBe(false)
    expect(button('Install macmon').querySelector('.spinner-element')).toBeNull()
})

it('shows unsupported-platform inline without a retry button', async () => {
    installMacmon.mockResolvedValue('unsupported-platform')
    await panel(readings({ macmon: 'missing' }))
    await click(button('Install macmon'))
    expect(text()).toContain("macmon isn't supported on this Mac")
    expect(text()).not.toContain('Install macmon')
})

it('waits for readings after install and clears that state when available', async () => {
    installMacmon.mockResolvedValue('installed')
    await panel(readings({ macmon: 'missing' }))
    await click(button('Install macmon'))
    expect(text()).toContain('Installed — waiting for readings…')
    await panel()
    expect(text()).not.toContain('waiting for readings')
    expect(text(tile('Temperature'))).toContain('36 °C')
    await panel(readings({ macmon: 'missing' }))
    expect(button('Install macmon').disabled).toBe(false)
})

it('renders Sensors not responding and no install control when macmon failed', async () => {
    await panel(readings({ macmon: 'failed', memoryPressure: 'critical' }))
    for (const label of ['Temperature', 'Fans', 'Power'])
        expect(text(tile(label))).toBe(`${label} —`)
    expect(text()).toContain('Sensors not responding')
    expect(text()).not.toContain('Install macmon')
    expect(text(tile('Memory'))).toContain('Critical')
})

it('renders unknown macmon data as dashes without an install offer', async () => {
    await panel(readings({ macmon: null }))
    for (const label of ['Temperature', 'Fans', 'Power'])
        expect(text(tile(label))).toBe(`${label} —`)
    expect(text()).not.toContain('Install macmon')
})

it.each(['missing', 'failed'])('keeps pressure badges working when macmon is %s', async status => {
    const sensors = readings({
        macmon: status === 'missing' ? 'missing' : 'failed',
        thermalPressure: 'heavy',
        memoryPressure: 'warning'
    })
    await render(createElement(NodeSensorBadges, { sensors }))
    expect(text()).toBe('Throttling Memory pressure')
})

it('never colours temperatures, even at 105 °C and critical pressure', async () => {
    await panel(
        readings({ cpuTempC: 105, thermalPressure: 'trapping', memoryPressure: 'critical' })
    )
    expect(text(tile('Temperature'))).toContain('105 °C')
    const elements = [tile('Temperature'), ...tile('Temperature').querySelectorAll('*')]
    for (const element of elements) {
        expect(element.className).not.toMatch(/amber|red|warning|danger/)
        expect(element.getAttribute('style') ?? '').not.toMatch(/color/)
    }
})

const thermalCases: { level: ThermalPressure | null; tone: string }[] = [
    { level: null, tone: 'default' },
    { level: 'nominal', tone: 'default' },
    { level: 'moderate', tone: 'amber' },
    { level: 'heavy', tone: 'amber' },
    { level: 'trapping', tone: 'red' },
    { level: 'sleeping', tone: 'red' }
]
const memoryCases: { level: MemoryPressure | null; tone: string; label: string }[] = [
    { level: null, tone: 'default', label: '—' },
    { level: 'normal', tone: 'default', label: 'Normal' },
    { level: 'warning', tone: 'amber', label: 'Warning' },
    { level: 'critical', tone: 'red', label: 'Critical' }
]

it.each(thermalCases)('maps thermal $level to $tone', ({ level, tone }) => {
    expect(thermalTone(level)).toBe(tone)
})
it.each(memoryCases)('maps memory $level to $tone', ({ level, tone }) => {
    expect(memoryTone(level)).toBe(tone)
})

it.each(thermalCases)(
    'shows Throttling only for non-nominal thermal $level',
    async ({ level, tone }) => {
        await render(
            createElement(NodeSensorBadges, { sensors: readings({ thermalPressure: level }) })
        )
        if (tone === 'default') expect(container.textContent).toBe('')
        else {
            expect(text()).toBe('Throttling')
            expect(container.innerHTML).toContain(
                tone === 'amber' ? '--text-color-feedback-warning' : '--text-color-feedback-danger'
            )
        }
    }
)

it.each(memoryCases)(
    'shows Memory pressure only at warning or critical: $level',
    async ({ level, tone, label }) => {
        await render(
            createElement(NodeSensorBadges, { sensors: readings({ memoryPressure: level }) })
        )
        if (tone === 'default') expect(container.textContent).toBe('')
        else {
            expect(text()).toBe('Memory pressure')
            expect(container.innerHTML).toContain(
                tone === 'amber' ? '--text-color-feedback-warning' : '--text-color-feedback-danger'
            )
        }
        await panel(readings({ memoryPressure: level }))
        expect(text(tile('Memory'))).toContain(`Memory ${label}`)
        const markup = tile('Memory').innerHTML
        if (tone === 'default') expect(markup).not.toMatch(/feedback-warning|feedback-danger/)
        else
            expect(markup).toContain(
                tone === 'amber' ? '--text-color-feedback-warning' : '--text-color-feedback-danger'
            )
    }
)

it('renders no badges without sensors', async () => {
    await render(createElement(NodeSensorBadges, { sensors: null }))
    expect(container.textContent).toBe('')
})
