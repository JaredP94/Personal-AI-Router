// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Button, Flex, Text } from '@nvidia/foundations-react-core'
import { formatPullProgressLabel } from '@/ui/utils/formatters'
import type { IncomingSyncRow } from '@/ui/types/model-manager'

import { ModelDownloadControls } from './ModelDownloadControls'

export function IncomingSyncPullRow({
    row,
    onCancel,
    onRetry
}: {
    row: IncomingSyncRow
    onCancel?: () => void
    onRetry?: () => void
}) {
    return (
        <Flex
            align="center"
            justify="between"
            gap="2"
            className="mt-2 min-w-0"
            title={row.rawModel}
        >
            <Flex align="center" gap="2" className="min-w-0">
                {row.status !== 'cancelled' && row.status !== 'interrupted' && (
                    <span
                        className="spinner-element"
                        role="status"
                        aria-label=""
                        style={{ margin: 0 }}
                    />
                )}
                <Flex align="center" wrap="wrap" gap="1" className="min-w-0">
                    <Text kind="body/semibold/sm" className="truncate">
                        {row.label}
                    </Text>
                    <Text
                        kind="body/regular/sm"
                        className="text-subtle-color whitespace-nowrap italic capitalize"
                    >
                        {row.engineType === 'llamacpp' && row.status === 'cancelled'
                            ? 'Cancelled'
                            : row.engineType === 'llamacpp' && row.status === 'interrupted'
                              ? 'Download interrupted — retry'
                              : formatPullProgressLabel(row)}
                    </Text>
                </Flex>
            </Flex>
            {onRetry && (row.status === 'cancelled' || row.status === 'interrupted') ? (
                <Button size="small" kind="secondary" onClick={onRetry}>
                    Retry
                </Button>
            ) : (
                <ModelDownloadControls {...row} onCancel={onCancel} />
            )}
        </Flex>
    )
}
