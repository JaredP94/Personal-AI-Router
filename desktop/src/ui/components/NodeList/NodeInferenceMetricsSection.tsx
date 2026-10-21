// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useMemo, useState } from 'react'
import { Button, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import { EngineDisplayNames } from '@/shared/constants/engines'
import type { EngineNativeView } from '@/shared/types/metrics'
import { isEngineType } from '@/shared/utils/engines'
import { useMetricsStore } from '@/ui/stores/metrics.store'
import { useNodesStore } from '@/ui/stores/nodes.store'
import { formatModelDisplayName } from '@/ui/utils/format-model-display-name'
import { formatRelativeTime } from '@/ui/utils/formatters'
import {
    buildEngineNativeModels,
    buildEngineNativeRows,
    buildInferenceMetricsRows,
    buildModelMetricsRows,
    isEngineNativeViewStale,
    type EngineNativeModelEntry,
    type EngineNativeRow,
    type InferenceMetricsRow,
    type ModelMetricsRows
} from '@/ui/utils/inference-metrics'

/**
 * The node's prefill/decode section: the rates the broker computed over its
 * rolling window, the mean time-to-first-token, and the tokens it served.
 *
 * A phase the engine never reported reads as "not reported" rather than as a
 * rate of zero — the presence counters in the snapshot's totals say which
 * measurements the engine made at all.
 *
 * The window is part of every label here: the node's rates are the broker's
 * 60-second window, while the totals below them and the per-model breakdown
 * are the totals the broker has kept since it started. A model that has gone
 * idle keeps a high ratio over the totals since the broker started while the
 * node's windowed rate falls to zero, and the two must not read as the same
 * measurement.
 *
 * The per-model breakdown sits behind a disclosure that is collapsed by
 * default: the numbers are free to compute, but "this node is slow" is
 * usually "this node is slow for this model", and a node that served several
 * models should not bury its headline numbers under a table.
 *
 * Below that, the engine's own view of itself is shown as a separate section:
 * the counters the engine published about its own work. They are the
 * engine's numbers, not PAIR's — they include the traffic PAIR did not route
 * — so they are headed as such, labelled with the engine's own scope, and
 * never merged with the rows above.
 */
function NodeInferenceMetricsSection({ nodeId }: { nodeId: string }) {
    // A narrow selector: this component re-renders when its own node's
    // measurement changes, not when any other node's does.
    const metrics = useMetricsStore(state => state.nodeInferenceMetrics.get(nodeId))
    // The engine's own view rides on the node record, so it is read from the
    // node store rather than from the metrics snapshot.
    const engineNative = useNodesStore(state => state.nodes.get(nodeId)?.engineNative)
    const [showModels, setShowModels] = useState(false)

    const rows = useMemo(() => (metrics ? buildInferenceMetricsRows(metrics) : []), [metrics])
    const modelRows = useMemo(
        (): ModelMetricsRows[] => (metrics ? buildModelMetricsRows(metrics.models) : []),
        [metrics]
    )
    const engineRows = useMemo(
        (): EngineNativeRow[] => (engineNative ? buildEngineNativeRows(engineNative) : []),
        [engineNative]
    )
    const engineModels = useMemo(
        (): EngineNativeModelEntry[] => (engineNative ? buildEngineNativeModels(engineNative) : []),
        [engineNative]
    )

    // A node that never served anything has no snapshot, and a node whose
    // engine publishes no native view has no engine section: an empty section
    // would read as a node that served nothing at zero speed.
    if (!metrics && engineRows.length === 0 && engineModels.length === 0) return null

    return (
        <Stack gap="1" className="mt-2 ml-1 min-w-0">
            {metrics && (
                <>
                    <Flex align="center" wrap="wrap" gap="3" className="min-w-0">
                        {rows.map(row => (
                            <MetricValue key={row.label} row={row} />
                        ))}
                    </Flex>
                    {modelRows.length > 0 && (
                        <Stack gap="1" className="min-w-0">
                            <Button
                                kind="tertiary"
                                color="neutral"
                                size="tiny"
                                className="self-start px-2"
                                onClick={() => setShowModels(open => !open)}
                                aria-expanded={showModels}
                                aria-label={
                                    showModels ? 'Hide per-model metrics' : 'Show per-model metrics'
                                }
                            >
                                {showModels ? 'Hide per-model metrics' : 'Show per-model metrics'}
                            </Button>
                            {showModels && (
                                <Stack gap="2" className="mt-1 min-w-0">
                                    {modelRows.map(entry => (
                                        <Stack key={entry.model} gap="0" className="min-w-0">
                                            <Text
                                                kind="body/semibold/sm"
                                                className="truncate"
                                                title={entry.model}
                                            >
                                                {formatModelDisplayName(entry.model)}
                                            </Text>
                                            <Flex
                                                align="center"
                                                wrap="wrap"
                                                gap="3"
                                                className="min-w-0"
                                            >
                                                {entry.rows.map(row => (
                                                    <MetricValue key={row.label} row={row} />
                                                ))}
                                            </Flex>
                                        </Stack>
                                    ))}
                                </Stack>
                            )}
                        </Stack>
                    )}
                </>
            )}
            {(engineRows.length > 0 || engineModels.length > 0) && (
                <Stack gap="1" className="mt-2 min-w-0">
                    <Text kind="body/semibold/sm">
                        {`Engine's own counters (${engineDisplayName(engineNative)})`}
                    </Text>
                    <Text kind="body/regular/sm" className="text-subtle-color">
                        {engineNativeSummary(engineNative)}
                    </Text>
                    {engineRows.length > 0 && (
                        <Flex align="center" wrap="wrap" gap="3" className="min-w-0">
                            {engineRows.map(row => (
                                <EngineNativeValue key={row.label} row={row} />
                            ))}
                        </Flex>
                    )}
                    {engineModels.length > 0 && (
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            {`Models the engine lists: ${engineModels
                                .map(model => engineModelLabel(model))
                                .join(', ')}`}
                        </Text>
                    )}
                </Stack>
            )}
        </Stack>
    )
}

/**
 * One measurement: its label and its value.
 *
 * An unavailable value is rendered in the subtle colour: the engine never
 * reported it, and it should not read as a measurement the engine made.
 */
function MetricValue({ row }: { row: InferenceMetricsRow }) {
    return (
        <Flex align="center" gap="1" className="min-w-0">
            <Text kind="body/regular/sm" className="text-subtle-color">
                {row.label}
            </Text>
            <Text
                kind="body/semibold/sm"
                className={row.unavailable ? 'text-subtle-color' : undefined}
            >
                {row.value}
            </Text>
        </Flex>
    )
}

/**
 * One of the engine's own counters.
 *
 * Every row here was reported by the engine, so there is no unavailable state:
 * a value the engine did not report has no row at all.
 */
function EngineNativeValue({ row }: { row: EngineNativeRow }) {
    return (
        <Flex align="center" gap="1" className="min-w-0">
            <Text kind="body/regular/sm" className="text-subtle-color">
                {row.label}
            </Text>
            <Text kind="body/semibold/sm">{row.value}</Text>
        </Flex>
    )
}

/**
 * The name of the engine that published the view.
 *
 * An engine PAIR knows gets its display name; an engine PAIR does not know
 * is shown under the name it published itself.
 */
function engineDisplayName(view: EngineNativeView | undefined): string {
    if (!view) return ''
    return isEngineType(view.engine) ? EngineDisplayNames[view.engine] : view.engine
}

/**
 * The line that says what the engine's own counters are, and how fresh they
 * are.
 *
 * The freshness matters: the stamp advances on every sweep, so a node whose
 * engine stopped answering keeps its last good view with a frozen stamp, and
 * this line is where that shows.
 */
function engineNativeSummary(view: EngineNativeView | undefined): string {
    const parts = ["These are the engine's own counters — they include traffic PAIR did not route."]
    const age = engineFreshness(view)
    if (age) parts.push(age)
    return parts.join(' ')
}

/**
 * How fresh the engine's own view is, as the card shows it.
 *
 * A view with no stamp carries no age: the engine published none, so there is
 * nothing to measure.
 */
function engineFreshness(view: EngineNativeView | undefined): string {
    if (!view || view.observedAt === undefined) return ''
    const observed = formatRelativeTime(view.observedAt)
    if (isEngineNativeViewStale(view, Date.now())) {
        return `Stale — last observed ${observed}.`
    }
    return `Observed ${observed}.`
}

/**
 * One model the engine lists, with the engine's own claim about it.
 *
 * The wording is the engine's: it lists the model as active or not. It is not
 * the engine-manager's loaded set, which says the model is resident in
 * memory — a different thing, kept under a different word.
 */
function engineModelLabel(model: EngineNativeModelEntry): string {
    const name = formatModelDisplayName(model.model)
    return model.active ? `${name} (active)` : `${name} (not active)`
}

export default NodeInferenceMetricsSection
