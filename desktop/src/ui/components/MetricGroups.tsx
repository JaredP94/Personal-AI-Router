// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Fragment } from 'react'
import { Text } from '@nvidia/foundations-react-core'
import type { MetricsGroup } from '@/ui/utils/inference-metrics'

/** Anything with a label and a value; an absent value is marked unavailable. */
interface MetricRow {
    label: string
    value: string
    unavailable?: boolean
}

/**
 * Groups of metrics, each headed once by the scope its rows cover.
 *
 * - `tiles` lays every group out as a row of label-over-value cells with a
 *   fixed minimum width, for wide cards where a horizontal strip keeps the
 *   section short and the values line up from card to card.
 * - `list` lays the rows out as one two-column grid shared by every group,
 *   so labels and values line up down the card, for narrow cards whose
 *   values are too long to sit side by side.
 */
export default function MetricGroups({
    groups,
    layout
}: {
    groups: MetricsGroup<MetricRow>[]
    layout: 'tiles' | 'list'
}) {
    if (layout === 'list') {
        return (
            <div className="grid grid-cols-[max-content_minmax(0,1fr)] gap-x-3 gap-y-1 min-w-0 leading-none">
                {groups.map((group, index) => (
                    <Fragment key={group.scope}>
                        <div className={`col-span-2 ${index > 0 ? 'mt-1.5' : ''}`}>
                            <ScopeHeading scope={group.scope} />
                        </div>
                        {group.note ? (
                            <Text kind="body/regular/sm" className="col-span-2 text-subtle-color">
                                {group.note}
                            </Text>
                        ) : (
                            <dl className="contents" data-metric-scope={group.scope}>
                                {group.rows.map(row => (
                                    <Fragment key={row.label}>
                                        <dt>
                                            <Text
                                                kind="body/regular/sm"
                                                className="text-subtle-color"
                                            >
                                                {row.label}
                                            </Text>
                                        </dt>
                                        <dd className="m-0 min-w-0">
                                            <MetricValue row={row} />
                                        </dd>
                                    </Fragment>
                                ))}
                            </dl>
                        )}
                    </Fragment>
                ))}
            </div>
        )
    }

    return (
        <div className="flex flex-wrap gap-x-8 gap-y-2 min-w-0">
            {groups.map(group => (
                <div key={group.scope} className="min-w-0">
                    <ScopeHeading scope={group.scope} />
                    {group.note ? (
                        <Text kind="body/regular/sm" className="block text-subtle-color">
                            {group.note}
                        </Text>
                    ) : (
                        <dl
                            className="m-0 mt-1 flex flex-wrap gap-x-5 gap-y-2 leading-none"
                            data-metric-scope={group.scope}
                        >
                            {group.rows.map(row => (
                                <div key={row.label} className="min-w-22">
                                    <dt>
                                        <Text
                                            kind="body/regular/xs"
                                            className="block leading-4 text-subtle-color"
                                        >
                                            {row.label}
                                        </Text>
                                    </dt>
                                    <dd className="m-0">
                                        <MetricValue row={row} />
                                    </dd>
                                </div>
                            ))}
                        </dl>
                    )}
                </div>
            ))}
        </div>
    )
}

/** The scope a group covers, said once above its rows. */
export function ScopeHeading({ scope }: { scope: string }) {
    return (
        <Text kind="body/semibold/xs" className="block uppercase tracking-wide text-subtle-color">
            {scope}
        </Text>
    )
}

/**
 * One value, in tabular figures so a live number does not shift as it
 * changes.
 *
 * An unavailable value shows as a dash in the subtle colour: the engine never
 * reported it, and it must not read as a measurement. The reason stays in the
 * accessible text and the tooltip.
 */
export function MetricValue({ row }: { row: MetricRow }) {
    if (row.unavailable) {
        return (
            <Text
                kind="body/semibold/sm"
                className="text-subtle-color whitespace-nowrap"
                title={row.value}
            >
                <span aria-hidden>—</span>
                <span className="sr-only">{row.value}</span>
            </Text>
        )
    }
    return (
        <Text kind="body/semibold/sm" className="tabular-nums">
            {row.value}
        </Text>
    )
}
