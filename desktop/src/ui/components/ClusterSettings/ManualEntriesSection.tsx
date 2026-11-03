// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useState } from 'react'
import { Button, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import type { ManualNodeStatus } from '@/shared/types/manual-nodes'
import getErrorString from '@/shared/utils/get-error-string'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'
import ManualNodeItem from './ManualNodeItem'

export default function ManualEntriesSection() {
    const [manualNodes, setManualNodes] = useState<ManualNodeStatus[]>([])
    const [loading, setLoading] = useState(false)
    const [error, setError] = useState<string | null>(null)

    const refresh = useCallback(async () => {
        if (!window.pairApi?.manualNodes) return
        try {
            setLoading(true)
            const list = await window.pairApi.manualNodes.list()
            setManualNodes(list)
            setError(null)
        } catch (err) {
            setError(getErrorString(err))
        } finally {
            setLoading(false)
        }
    }, [])

    useEffect(() => {
        void refresh()
    }, [refresh])

    return (
        <Stack gap="2">
            <Flex align="center" justify="between" className="px-1">
                <Text kind="body/semibold/md">Manual entries</Text>
                <Button
                    kind="tertiary"
                    size="small"
                    disabled={loading}
                    onClick={() => void refresh()}
                >
                    Refresh
                </Button>
                {loading && (
                    <span
                        className="spinner-element"
                        role="status"
                        aria-label="Loading manual entries"
                    />
                )}
            </Flex>

            {error && <InlineErrorBanner severity="error" message={error} />}

            {manualNodes.length === 0 ? (
                <div className="settings-card pair-paper p-4">
                    <Text kind="body/regular/sm" className="text-subtle-color">
                        No manual entries added yet. Use &ldquo;Add node&rdquo; to add a PAIR
                        machine or bare engine by address.
                    </Text>
                </div>
            ) : (
                manualNodes.map(node => (
                    <ManualNodeItem
                        key={node.id}
                        node={node}
                        onChanged={refresh}
                        onError={setError}
                    />
                ))
            )}
        </Stack>
    )
}
