// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Flex, Text } from '@nvidia/foundations-react-core'
import type { ModelItem } from '@/ui/types/engine-info'
import { formatPullProgressLabel } from '@/ui/utils/formatters'
import type { EngineProgress } from '@/shared/types/engines'

import { ModelDownloadControls } from './ModelDownloadControls'

export function TransientModelStatusRow({
    transientModel,
    displayName,
    pullProgress,
    onCancel
}: {
    transientModel: ModelItem
    displayName: (name: string) => string
    pullProgress: EngineProgress | undefined
    onCancel?: () => void
}) {
    return (
        <Flex align="center" justify="between" gap="2" className="mt-2 min-w-0">
            <Flex align="center" gap="1" className="min-w-0">
                {pullProgress?.status !== 'cancelled' && pullProgress?.status !== 'interrupted' && (
                    <span
                        className="spinner-element"
                        role="status"
                        aria-label=""
                        style={{ margin: 0 }}
                    />
                )}
                <Text kind="body/semibold/sm" className="truncate">
                    {transientModel.status === 'loading' &&
                        `Loading ${displayName(transientModel.name)}...`}
                    {transientModel.status === 'ejecting' &&
                        `Ejecting ${displayName(transientModel.name)}...`}
                    {transientModel.status === 'pulling' &&
                        `${displayName(transientModel.name)}${
                            pullProgress && pullProgress.status !== 'idle'
                                ? ` · ${formatPullProgressLabel(pullProgress)}`
                                : '...'
                        }`}
                </Text>
            </Flex>
            <ModelDownloadControls {...pullProgress} onCancel={onCancel} />
        </Flex>
    )
}
