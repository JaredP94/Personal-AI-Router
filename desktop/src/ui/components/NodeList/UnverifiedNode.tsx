// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { NodeItem } from '@/shared/types/nodes'
import AvailableNode from '@/ui/components/ClusterSettings/AvailableNode'

/** Preview/configuration rows never mount peer telemetry or engine controls. */
export default function UnverifiedNode({ node }: { node: NodeItem }) {
    return (
        <div data-node-id={node.id}>
            <AvailableNode node={{ ...node, trusted: false, clustered: false, lastSeen: 0 }} />
        </div>
    )
}
