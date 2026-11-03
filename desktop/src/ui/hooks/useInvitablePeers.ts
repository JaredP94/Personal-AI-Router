// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useMemo } from 'react'
import { useShallow } from 'zustand/react/shallow'
import { nodeRecordKey } from '@/shared/utils/node-record-key'
import type { AvailableNode } from '@/shared/types/cluster'
import { useDiscoveredNodesStore } from '@/ui/stores/discovered-nodes.store'
import { useClusterInvitationsStore } from '@/ui/stores/cluster-invitations.store'
import { useConnectionStore } from '@/ui/stores/connection.store'

/**
 * Discovered LAN peers that can be invited: everything the broker has discovered
 * minus manual engines, current cluster members and self (by record key).
 *
 * This is the single source for both the Add Node modal and the cluster settings
 * "Available Nodes to Add" list, so the two can never disagree (the modal used
 * to filter against the overview node store, which is membership-scoped, leaving
 * it perpetually empty).
 */
export function useInvitablePeers(): AvailableNode[] {
    const discoveredNodes = useDiscoveredNodesStore(useShallow(state => state.nodes))
    const members = useClusterInvitationsStore(useShallow(state => state.members))
    const selfId = useConnectionStore(state => state.selfId)

    return useMemo(() => {
        // Only proven peer record keys correlate with member UUIDs/self. Preview
        // keys remain separate even when their claimed UUID names a member.
        const memberUuids = new Set(members.map(m => m.nodeUuid))
        return discoveredNodes.filter(node => {
            const key = nodeRecordKey(node)
            return (
                key !== '' &&
                node.identityVerification !== 'manual' &&
                !key.startsWith('manual:') &&
                !memberUuids.has(key) &&
                key !== selfId
            )
        })
    }, [discoveredNodes, members, selfId])
}
