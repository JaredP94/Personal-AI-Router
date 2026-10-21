// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useMemo, useState } from 'react'
import { Button, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import { useMetricsStore } from '@/ui/stores/metrics.store'
import { formatModelDisplayName } from '@/ui/utils/format-model-display-name'
import {
    buildInferenceMetricsRows,
    buildModelMetricsRows,
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
 * idle keeps a high lifetime ratio while the node's windowed rate falls to
 * zero, and the two must not read as the same measurement.
 *
 * The per-model breakdown sits behind a disclosure that is collapsed by
 * default: the numbers are free to compute, but "this node is slow" is
 * usually "this node is slow for this model", and a node that served several
 * models should not bury its headline numbers under a table.
 */
function NodeInferenceMetricsSection({ nodeId }: { nodeId: string }) {
    // A narrow selector: this component re-renders when its own node's
    // measurement changes, not when any other node's does.
    const metrics = useMetricsStore(state => state.nodeInferenceMetrics.get(nodeId))
    const [showModels, setShowModels] = useState(false)

    const rows = useMemo(() => (metrics ? buildInferenceMetricsRows(metrics) : []), [metrics])
    const modelRows = useMemo(
        (): ModelMetricsRows[] => (metrics ? buildModelMetricsRows(metrics.models) : []),
        [metrics]
    )

    // A node that never served anything has no snapshot and no section: an
    // empty section would read as a node that served nothing at zero speed.
    if (!metrics) return null

    return (
        <Stack gap="1" className="mt-2 ml-1 min-w-0">
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
                                    <Flex align="center" wrap="wrap" gap="3" className="min-w-0">
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

export default NodeInferenceMetricsSection
