// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Fragment, useCallback, useMemo, useState } from 'react'
import { Flex, Text } from '@nvidia/foundations-react-core'
import SvgLineChart, { type MetricDataset } from '@/ui/components/SvgLineChart'
import type { NodeItem } from '@/shared/types/nodes'
import { useMetricsStore } from '@/ui/stores/metrics.store'
import { getGpuColor, getVramColor } from '@/ui/utils/colors'
import { formatBytes } from '@/ui/utils/formatters'
import { CHART_COLORS } from '@/ui/constants/colors'
import { getGpuMemoryLabel, getGpuMemoryPresentation } from '@/ui/utils/gpu-memory'

export default function NodePerformance({
    node,
    className
}: {
    node: NodeItem
    className?: string
}) {
    const nodeMetrics = useMetricsStore(state => state.nodeMetrics.get(node.id))
    const [selectedMetric, setSelectedMetric] = useState<string | undefined>(undefined)

    const handleLegendClick = useCallback((metricKey: string) => {
        // If clicking the same metric that's already selected, show all
        // Otherwise, show only the clicked metric (solo it)
        setSelectedMetric(prev => (prev === metricKey ? undefined : metricKey))
    }, [])

    // A series is shown when nothing is soloed or it is the soloed one.
    const isShown = (metricKey: string) =>
        selectedMetric === undefined || selectedMetric === metricKey

    // The detailed performance view intentionally charts EVERY detected GPU,
    // including any the radial omits (the radial filters to inference-ready GPUs
    // via inferenceHardwareIds, src/ui/utils/gpu-inference.ts). Here the user is
    // inspecting raw hardware activity, so an iGPU's utilization is useful signal
    // rather than noise.
    const datasets = useMemo(() => {
        if (!nodeMetrics) return []

        const gpuCount = nodeMetrics.gpuUtilization.length
        const result: MetricDataset[] = []

        // Add GPU utilization datasets (cycling through color palette)
        nodeMetrics.gpuUtilization.forEach((gpu, index) => {
            const color = getGpuColor(index)
            result.push({
                data: gpu.data,
                label: gpuCount > 1 ? `GPU ${index}` : 'GPU',
                color,
                key: `gpu-${gpu.id}`
            })
        })

        // Add CPU
        result.push({
            data: nodeMetrics.cpuUtilization,
            label: 'CPU',
            color: CHART_COLORS.CPU,
            key: 'cpu'
        })

        // Add RAM
        result.push({
            data: nodeMetrics.memoryUsage,
            label: 'RAM',
            color: CHART_COLORS.MEMORY,
            key: 'memory'
        })

        // Memory remains per adapter; unavailable memory has no numeric series.
        nodeMetrics.gpuVramUsage.forEach((gpu, index) => {
            const hardware = node.topology.gpus[index]
            if (!hardware || hardware.memoryKind === 'unavailable') return
            const color = getVramColor(index)
            const label = getGpuMemoryLabel(hardware.memoryKind)
            result.push({
                data: gpu.data,
                label: gpuCount > 1 ? `${label} ${index}` : label,
                color,
                key: `vram-${gpu.id}`
            })
        })

        return result
    }, [nodeMetrics, node.topology.gpus])

    const hardwareInfo = useMemo(() => {
        const totalStorage = node.topology.storage.reduce(
            (sum, storage) => sum + storage.capacity,
            0
        )

        // Helper to get the latest value from a metric data array
        const getLatestValue = (data: { timestamp: number; value: number | null }[]) =>
            data[data.length - 1]?.value ?? null

        return {
            // Detailed view lists every detected GPU (incl. iGPUs); only the
            // radial filters to inference-ready GPUs (via inferenceHardwareIds).
            gpus: node.topology.gpus.map((gpu, index) => {
                const gpuUtilData = nodeMetrics?.gpuUtilization[index]
                const gpuVramData = nodeMetrics?.gpuVramUsage[index]

                return {
                    id: gpu.id,
                    name: gpu.name,
                    memory: getGpuMemoryPresentation(
                        gpu,
                        gpuVramData ? getLatestValue(gpuVramData.data) : null,
                        gpuVramData ? getLatestValue(gpuVramData.sharedUsed) : null
                    ),
                    utilization: gpuUtilData ? getLatestValue(gpuUtilData.data) : null,
                    color: getGpuColor(index),
                    vramColor: getVramColor(index)
                }
            }),
            cpuCores: node.topology.cpu.cores,
            cpuModel: node.topology.cpu.model,
            cpuUtilization: nodeMetrics ? (getLatestValue(nodeMetrics.cpuUtilization) ?? 0) : 0,
            memory: formatBytes(node.topology.ram, 1),
            memoryTotal: node.topology.ram,
            memoryUsage: nodeMetrics ? (getLatestValue(nodeMetrics.memoryUsage) ?? 0) : 0,
            storage: formatBytes(totalStorage, 1)
        }
    }, [node, nodeMetrics])

    return (
        <Flex
            className={`w-full min-w-0 flex-nowrap max-[1000px]:flex-wrap ${className ? className : ''}`}
            align="stretch"
            gap="2"
        >
            {/* Chart Section (placeholder when no metrics) */}
            <div className="grow min-w-0 overflow-hidden min-h-[200px] relative">
                <SvgLineChart
                    datasets={datasets}
                    maxDataPoints={30}
                    yAxisMax={100}
                    selectedKey={selectedMetric}
                />
                {!nodeMetrics && (
                    <div className="absolute inset-0 flex items-center justify-center pointer-events-none">
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            No metrics yet
                        </Text>
                    </div>
                )}
            </div>

            {/* Legend: one aligned grid of dot, label and value. A row solos its series. */}
            <div className="shrink-0 w-64 ml-2 self-center grid grid-cols-[auto_max-content_minmax(0,1fr)] items-baseline gap-x-2">
                {hardwareInfo.gpus.map((gpu, index) => {
                    const usageKey = `gpu-${gpu.id}`
                    const vramKey = `vram-${gpu.id}`
                    return (
                        <Fragment key={gpu.id}>
                            <LegendHeading
                                title={gpu.name}
                                first={index === 0}
                                dimmed={!isShown(usageKey) && !isShown(vramKey)}
                            />
                            <LegendRow
                                color={gpu.color}
                                label="Usage"
                                value={
                                    gpu.utilization === null
                                        ? '—'
                                        : `${Math.floor(gpu.utilization)}%`
                                }
                                shown={isShown(usageKey)}
                                onClick={() => handleLegendClick(usageKey)}
                            />
                            <LegendRow
                                color={gpu.vramColor}
                                label={gpu.memory.label}
                                value={gpu.memory.detail ?? ''}
                                shown={isShown(vramKey)}
                                onClick={() => handleLegendClick(vramKey)}
                            />
                            {gpu.memory.sharedDetail && (
                                <Text
                                    kind="body/regular/sm"
                                    className="col-start-2 col-span-2 text-subtle-color"
                                >
                                    {gpu.memory.sharedDetail}
                                </Text>
                            )}
                        </Fragment>
                    )
                })}

                <LegendHeading
                    title={hardwareInfo.cpuModel}
                    first={hardwareInfo.gpus.length === 0}
                    dimmed={!isShown('cpu')}
                />
                <LegendRow
                    color={CHART_COLORS.CPU}
                    label="Usage"
                    value={`${Math.floor(hardwareInfo.cpuUtilization)}%`}
                    shown={isShown('cpu')}
                    onClick={() => handleLegendClick('cpu')}
                />
                {hardwareInfo.cpuCores ? (
                    <>
                        <Text kind="body/regular/sm" className="col-start-2 text-subtle-color">
                            Cores
                        </Text>
                        <Text kind="body/regular/sm" className="text-right tabular-nums">
                            {hardwareInfo.cpuCores}
                        </Text>
                    </>
                ) : null}

                <LegendHeading title="Memory" first={false} dimmed={!isShown('memory')} />
                <LegendRow
                    color={CHART_COLORS.MEMORY}
                    label="RAM"
                    value={`${formatBytes(
                        Math.floor((hardwareInfo.memoryTotal * hardwareInfo.memoryUsage) / 100),
                        1
                    )} / ${hardwareInfo.memory}`}
                    shown={isShown('memory')}
                    onClick={() => handleLegendClick('memory')}
                />
            </div>
        </Flex>
    )
}

/** The hardware a block of legend rows belongs to: a GPU, the CPU, or memory. */
function LegendHeading({
    title,
    first,
    dimmed
}: {
    title: string
    first: boolean
    dimmed: boolean
}) {
    return (
        <Text
            kind="body/semibold/sm"
            className={`col-span-3 truncate transition-opacity ${first ? '' : 'mt-2'}`}
            style={{ opacity: dimmed ? 0.5 : 1 }}
            title={title}
        >
            {title}
        </Text>
    )
}

/**
 * One charted series: its colour, label and latest value. The row spans the
 * legend's three columns through a subgrid, so dots, labels and values line
 * up across every block. Clicking it solos the series; clicking again shows
 * them all.
 */
function LegendRow({
    color,
    label,
    value,
    shown,
    onClick
}: {
    color: string
    label: string
    value: string
    shown: boolean
    onClick: () => void
}) {
    return (
        <button
            type="button"
            className="col-span-3 grid grid-cols-subgrid items-baseline text-left text-inherit cursor-pointer bg-transparent border-0 p-0 transition-opacity"
            style={{ opacity: shown ? 1 : 0.5 }}
            onClick={onClick}
        >
            <span
                aria-hidden
                className="size-2.5 self-center rounded-full"
                style={{ backgroundColor: `${color}BF`, border: `2px solid ${color}` }}
            />
            <Text kind="body/regular/sm" className="text-subtle-color">
                {label}
            </Text>
            <Text kind="body/regular/sm" className="text-right tabular-nums">
                {value}
            </Text>
        </button>
    )
}
