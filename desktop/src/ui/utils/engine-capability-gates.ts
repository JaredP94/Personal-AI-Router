// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * UI gates derived from the capabilities an owning node's engine manager
 * reports for one engine (`EngineStatusData.capabilities`).
 *
 * Every gate takes the flags of the node the row belongs to, so a remote node's
 * row is gated by what that peer reported rather than by a table of what an
 * engine name usually supports locally. Nothing here inspects the engine type:
 * oMLX gets no Delete and no editable port because its own node reports no
 * `delete_model` and no `{port}` in its start command, not because a constant
 * says so.
 */
import type { EngineCapabilityFlags } from '@/shared/types/engines'
import type { EngineCaps } from '@/ui/types/engine-manifest'

interface ModelActionItem {
    id: 'load' | 'eject' | 'delete'
    children: string
    disabled?: boolean
    danger?: boolean
}

interface ModelActionInput {
    /** Static UI metadata (which controls exist at all, e.g. Eject). */
    capabilities: EngineCaps
    /** What the node that owns this model reports. */
    reported: EngineCapabilityFlags
    isRunning: boolean
    isLoaded: boolean
    /** The model file is present on disk. */
    downloaded: boolean
    /** A command for this model is already in flight. */
    busy: boolean
}

/**
 * The model action menu for one row. Load/Eject come from static UI metadata
 * plus live model state; Delete additionally requires the owning node to report
 * a `delete_model` action, so an engine that cannot delete (oMLX) never offers
 * it. While a command is in flight the whole menu locks, which also stops a
 * second conflicting op from being fired.
 */
export function modelActionItems({
    capabilities,
    reported,
    isRunning,
    isLoaded,
    downloaded,
    busy
}: ModelActionInput): ModelActionItem[] {
    const items: ModelActionItem[] = []
    items.push({
        id: 'load',
        children: 'Load',
        disabled: busy || isLoaded || !downloaded || !isRunning
    })
    if (capabilities.hasEject) {
        items.push({
            id: 'eject',
            children: 'Eject',
            // The backend reports per-model loaded state and stamps
            // `status: 'loaded'`, so Eject is offered only for a model that is
            // actually resident in memory — ejecting an idle model would be a
            // no-op.
            disabled: busy || !isRunning || !isLoaded
        })
    }
    if (reported.deleteModel) {
        items.push({ id: 'delete', children: 'Delete', danger: true, disabled: busy })
    }
    return items
}
