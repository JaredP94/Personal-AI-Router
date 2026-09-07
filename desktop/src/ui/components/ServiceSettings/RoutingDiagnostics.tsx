// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Flex, Stack, Text } from '@nvidia/foundations-react-core'
import { EngineDisplayNames } from '@/shared/constants/engines'
import { useMetricsStore } from '@/ui/stores/metrics.store'

export default function RoutingDiagnostics() {
    const metrics = useMetricsStore(state => state.routingMetrics)
    return (
        <div className="settings-card settings-card-stacked pair-paper p-4">
            <Stack gap="4">
                <Text kind="body/semibold/md">Cache affinity selections</Text>
                <Text kind="body/regular/sm" className="text-subtle-color">
                    Affinity selections among completed inference requests observed during this
                    desktop session. This measures routing decisions, not engine KV-cache hits.
                </Text>
                {metrics.map(row => (
                    <Flex key={row.engineType} justify="between" gap="4">
                        <Text kind="body/regular/sm">{EngineDisplayNames[row.engineType]}</Text>
                        <Text kind="body/regular/sm">
                            {row.requests === 0
                                ? 'No requests'
                                : `${((100 * row.cacheAffinityHits) / row.requests).toFixed(1)}% (${row.cacheAffinityHits}/${row.requests})`}
                        </Text>
                    </Flex>
                ))}
            </Stack>
        </div>
    )
}
