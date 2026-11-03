// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useState } from 'react'
import { Badge, Button, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import type { ManualNodeStatus } from '@/shared/types/manual-nodes'
import { InvitePairingPanel } from '@/ui/components/InvitePairingPanel'
import { useInvitePairing } from '@/ui/hooks/useInvitePairing'
import getErrorString from '@/shared/utils/get-error-string'

interface ManualNodeItemProps {
    node: ManualNodeStatus
    onChanged: () => void
    onError: (error: string | null) => void
}

export default function ManualNodeItem({ node, onChanged, onError }: ManualNodeItemProps) {
    const [removing, setRemoving] = useState(false)
    const pairing = useInvitePairing()
    const showPairing = pairing.invite !== null || pairing.error !== null
    const inviteInFlight = pairing.submitting || pairing.invite?.state === 'pending'
    const isVerified = node.identityVerification === 'verified'

    const handleRemove = async () => {
        try {
            setRemoving(true)
            onError(null)
            const result = await window.pairApi.manualNodes.remove(node.id)
            if (!result.removed) {
                onError(`Failed to remove manual node ${node.id}`)
                return
            }
            onChanged()
        } catch (err) {
            onError(getErrorString(err))
        } finally {
            setRemoving(false)
        }
    }

    if (showPairing) {
        return (
            <div className="settings-card node-item-card pair-paper p-4">
                <InvitePairingPanel
                    invite={pairing.invite}
                    error={pairing.error}
                    onReset={pairing.reset}
                    onCancel={() => void pairing.cancel()}
                    onDone={() => {
                        pairing.reset()
                        onChanged()
                    }}
                />
            </div>
        )
    }

    return (
        <div className="settings-card node-item-card pair-paper p-4">
            <Flex align="center" justify="between" gap="3">
                <Stack gap="1" className="min-w-0 flex-1">
                    <Flex align="center" gap="2" wrap="wrap">
                        <Text kind="body/semibold/sm" className="uppercase">
                            {node.name || node.address}
                        </Text>
                        <Badge color={node.kind === 'pair' ? 'teal' : 'gray'} kind="solid">
                            {node.kind === 'pair' ? 'PAIR machine' : 'Engine only'}
                        </Badge>
                        {node.kind === 'pair' ? (
                            <Badge color={isVerified ? 'green' : 'yellow'} kind="solid">
                                {isVerified ? 'Verified' : 'Unverified'}
                            </Badge>
                        ) : (
                            <Badge color="purple" kind="solid">
                                Manual bypass
                            </Badge>
                        )}
                    </Flex>
                    <Text kind="body/regular/sm" className="text-subtle-color">
                        {node.address}
                    </Text>
                </Stack>

                <Flex align="center" gap="2">
                    {node.kind === 'pair' && !isVerified && (
                        <Button
                            kind="primary"
                            color="brand"
                            size="small"
                            onClick={() => void pairing.start(node.address)}
                            disabled={inviteInFlight || removing}
                        >
                            Retry pairing
                        </Button>
                    )}
                    <Button
                        kind="secondary"
                        color="danger"
                        size="small"
                        onClick={() => void handleRemove()}
                        disabled={inviteInFlight || removing}
                    >
                        Remove
                    </Button>
                </Flex>
            </Flex>
        </div>
    )
}
