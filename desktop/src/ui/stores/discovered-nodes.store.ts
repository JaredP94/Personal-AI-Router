// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { nodeRecordKey } from '@/shared/utils/node-record-key'
import { create } from 'zustand'
import type { AvailableNode } from '@/shared/types/cluster'

interface DiscoveredNodesStore {
    nodes: AvailableNode[]
    initialize: () => Promise<void>
    refresh: () => Promise<void>
    cleanup: () => void
}

let unsubs: Array<() => void> = []

function validNodes(nodes: AvailableNode[]): AvailableNode[] {
    const records = new Map<string, AvailableNode>()
    for (const node of nodes) {
        const id = nodeRecordKey(node)
        if (id) records.set(id, { ...node, id })
    }
    return [...records.values()]
}

export const useDiscoveredNodesStore = create<DiscoveredNodesStore>(set => ({
    nodes: [],

    initialize: async () => {
        await useDiscoveredNodesStore.getState().refresh()

        if (!window.pairApi) return
        unsubs.push(
            window.pairApi.discovery.onNodesChanged(nodes => {
                set({ nodes: validNodes(nodes) })
            })
        )
    },

    refresh: async () => {
        try {
            const nodes = await window.pairApi.discovery.getNodes()
            set({ nodes: validNodes(nodes) })
        } catch {
            set({ nodes: [] })
        }
    },

    cleanup: () => {
        unsubs.forEach(u => u())
        unsubs = []
        set({ nodes: [] })
    }
}))
