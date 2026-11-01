// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useMemo, useState } from 'react'
import { Button, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import { EngineDisplayNames } from '@/shared/constants/engines'
import type { EngineNativeView } from '@/shared/types/metrics'
import { isEngineType } from '@/shared/utils/engines'
import { KeyboardArrowRight } from '@/ui/components/icons'
import MetricGroups, { MetricValue, ScopeHeading } from '@/ui/components/MetricGroups'
import { useMetricsStore } from '@/ui/stores/metrics.store'
import { useNodesStore } from '@/ui/stores/nodes.store'
import { formatModelDisplayName } from '@/ui/utils/format-model-display-name'
import { formatRelativeTime } from '@/ui/utils/formatters'
import {
    buildEngineNativeGroups,
    buildEngineNativeModels,
    buildModelMetricsRows,
    buildNodeMetricsGroups,
    isEngineNativeViewStale,
    TOTAL_SCOPE,
    type EngineNativeModelEntry,
    type EngineNativeRow,
    type InferenceMetricsRow,
    type MetricsGroup,
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
 * The window heads every group here: the node's rates are the broker's
 * 60-second window, while the token count beside them and the per-model
 * breakdown are the totals the broker has kept since it started. A model that
 * has gone idle keeps a high ratio over the totals since the broker started
 * while the node's windowed rate falls to zero, and the two must not read as
 * the same measurement.
 *
 * The per-model breakdown is a table behind a disclosure that is collapsed by
 * default: the numbers are free to compute, but "this node is slow" is
 * usually "this node is slow for this model", and a node that served several
 * models should not bury its headline numbers under a table.
 *
 * Below that, the engine's own view of itself is shown as a separate section:
 * the counters the engine published about its own work. They are the
 * engine's numbers, not PAIR's — they include the traffic PAIR did not route
 * — so they are headed as such, grouped under the engine's own scopes, and
 * never merged with the groups above.
 */
function NodeInferenceMetricsSection({ nodeId }: { nodeId: string }) {
    // A narrow selector: this component re-renders when its own node's
    // measurement changes, not when any other node's does.
    const metrics = useMetricsStore(state => state.nodeInferenceMetrics.get(nodeId))
    // The engine's own view rides on the node record, so it is read from the
    // node store rather than from the metrics snapshot.
    const engineNative = useNodesStore(state => state.nodes.get(nodeId)?.engineNative)
    const [showModels, setShowModels] = useState(false)

    const groups = useMemo(
        (): MetricsGroup<InferenceMetricsRow>[] => (metrics ? buildNodeMetricsGroups(metrics) : []),
        [metrics]
    )
    const modelRows = useMemo(
        (): ModelMetricsRows[] => (metrics ? buildModelMetricsRows(metrics.models) : []),
        [metrics]
    )
    const engineGroups = useMemo(
        (): MetricsGroup<EngineNativeRow>[] =>
            engineNative ? buildEngineNativeGroups(engineNative) : [],
        [engineNative]
    )
    const engineModels = useMemo(
        (): EngineNativeModelEntry[] => (engineNative ? buildEngineNativeModels(engineNative) : []),
        [engineNative]
    )

    // A node that never served anything has no snapshot, and a node whose
    // engine publishes no native view has no engine section: an empty section
    // would read as a node that served nothing at zero speed. A node whose
    // engine DID publish a view keeps its section even when every counter in
    // it is zero — the section is where the freshness of the view shows, and
    // a view of zeros is still a view.
    if (!metrics && !engineNative?.supported) return null

    return (
        <Stack gap="3" className="mt-1 ml-1 min-w-0">
            {metrics && (
                <Stack gap="1" className="min-w-0">
                    <MetricGroups groups={groups} layout="tiles" />
                    {modelRows.length > 0 && (
                        <>
                            <Button
                                kind="tertiary"
                                color="neutral"
                                size="tiny"
                                className="self-start px-1 -ml-1"
                                onClick={() => setShowModels(open => !open)}
                                aria-expanded={showModels}
                                aria-label={
                                    showModels ? 'Hide per-model metrics' : 'Show per-model metrics'
                                }
                            >
                                <KeyboardArrowRight
                                    aria-hidden
                                    style={{ fontSize: 14 }}
                                    className={`transition-transform ${showModels ? 'rotate-90' : ''}`}
                                />
                                Per model
                            </Button>
                            {showModels && <ModelMetricsTable models={modelRows} />}
                        </>
                    )}
                </Stack>
            )}
            {engineNative?.supported && (
                <Stack
                    gap="2"
                    className={`min-w-0 ${metrics ? 'pt-2 border-t border-[var(--border-color-base)]' : ''}`}
                >
                    <Flex align="baseline" wrap="wrap" gap="2" className="min-w-0">
                        <Text kind="body/semibold/sm">
                            {`Engine's own counters (${engineDisplayName(engineNative)})`}
                        </Text>
                        <Text kind="body/regular/xs" className="text-subtle-color">
                            Includes traffic PAIR did not route
                        </Text>
                        <EngineFreshness view={engineNative} />
                    </Flex>
                    {engineGroups.length > 0 && (
                        <MetricGroups groups={engineGroups} layout="tiles" />
                    )}
                    {engineModels.length > 0 && <EngineModelList models={engineModels} />}
                </Stack>
            )}
        </Stack>
    )
}

/**
 * The per-model breakdown: one row per model, one column per measurement,
 * every figure covering the totals since the broker started.
 *
 * Every model carries the same labels in the same order, so the first
 * model's labels are the column headers.
 */
function ModelMetricsTable({ models }: { models: ModelMetricsRows[] }) {
    const columns = models[0]?.rows.map(row => row.label) ?? []
    return (
        <div className="min-w-0 overflow-x-auto">
            <ScopeHeading scope={TOTAL_SCOPE} />
            <table className="mt-0.5 border-collapse text-left leading-none">
                <thead>
                    <tr>
                        <th scope="col" className="pr-4 pb-0.5 text-left font-normal">
                            <Text kind="body/regular/xs" className="text-subtle-color">
                                Model
                            </Text>
                        </th>
                        {columns.map(column => (
                            <th
                                key={column}
                                scope="col"
                                className="pl-4 pb-0.5 text-right font-normal whitespace-nowrap"
                            >
                                <Text kind="body/regular/xs" className="text-subtle-color">
                                    {column}
                                </Text>
                            </th>
                        ))}
                    </tr>
                </thead>
                <tbody>
                    {models.map(entry => (
                        <tr
                            key={entry.model}
                            className="border-t border-[var(--border-color-base)]"
                        >
                            <th scope="row" className="py-0.5 pr-4 text-left font-normal">
                                <Text
                                    kind="body/regular/sm"
                                    className="block max-w-56 truncate"
                                    title={entry.model}
                                >
                                    {formatModelDisplayName(entry.model)}
                                </Text>
                            </th>
                            {entry.rows.map(row => (
                                <td
                                    key={row.label}
                                    className="py-0.5 pl-4 text-right whitespace-nowrap"
                                >
                                    <MetricValue row={row} />
                                </td>
                            ))}
                        </tr>
                    ))}
                </tbody>
            </table>
        </div>
    )
}

/**
 * The models the engine lists, each marked with the engine's own claim:
 * a filled dot for a model it lists as active, a hollow one for the rest.
 */
function EngineModelList({ models }: { models: EngineNativeModelEntry[] }) {
    return (
        <div className="min-w-0">
            <ScopeHeading scope="Models the engine lists" />
            <Flex align="center" wrap="wrap" gap="3" className="mt-0.5 min-w-0">
                {models.map(model => {
                    const name = formatModelDisplayName(model.model)
                    const claim = engineModelClaim(model)
                    return (
                        <span
                            key={model.model}
                            className="inline-flex min-w-0 items-center gap-1.5"
                            title={`${model.model} — ${claim}`}
                        >
                            <span
                                aria-hidden
                                className="size-2 shrink-0 rounded-full border"
                                style={{
                                    borderColor: model.active
                                        ? 'var(--text-color-feedback-success)'
                                        : 'var(--border-color-interaction-strong)',
                                    backgroundColor: model.active
                                        ? 'var(--text-color-feedback-success)'
                                        : 'transparent'
                                }}
                            />
                            <Text
                                kind="body/regular/sm"
                                className={`truncate ${model.active ? '' : 'text-subtle-color'}`}
                            >
                                {name}
                            </Text>
                            <span className="sr-only">{`(${claim})`}</span>
                        </span>
                    )
                })}
            </Flex>
        </div>
    )
}

/**
 * How fresh the engine's own view is.
 *
 * The freshness matters: the stamp advances on every sweep, so a node whose
 * engine stopped answering keeps its last good view with a frozen stamp, and
 * this is where that shows. A stale view is said in the warning colour.
 *
 * A view with no stamp carries no age: the engine published none, so there is
 * nothing to measure.
 */
function EngineFreshness({ view }: { view: EngineNativeView }) {
    if (view.observedAt === undefined) return null
    const observed = formatRelativeTime(view.observedAt)
    if (isEngineNativeViewStale(view, Date.now())) {
        return (
            <Text kind="body/semibold/xs" style={{ color: 'var(--text-color-feedback-warning)' }}>
                {`Stale — last observed ${observed}`}
            </Text>
        )
    }
    return (
        <Text kind="body/regular/xs" className="text-subtle-color">
            {`· Observed ${observed}`}
        </Text>
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
 * The engine's own claim about one model it lists.
 *
 * The wording is the engine's: it lists the model as active or not. It is not
 * the engine-manager's loaded set, which says the model is resident in
 * memory — a different thing, kept under a different word.
 */
function engineModelClaim(model: EngineNativeModelEntry): string {
    return model.active ? 'active' : 'not active'
}

export default NodeInferenceMetricsSection
