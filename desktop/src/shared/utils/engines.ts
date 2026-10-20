// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { EngineTypes, ModelExpiries } from '@/shared/constants/engines'
import type { JsonValue } from '@/shared/types/json'
import {
    EngineModels,
    EngineStatusData,
    EngineType,
    EngineCapabilityFlags,
    ModelExpiry
} from '@/shared/types/engines'

const ENGINE_TYPE_SET: ReadonlySet<string> = new Set(EngineTypes)
/**
 * Runtime type guard — narrows a `string` (typically arriving from service
 * bridge payloads or persisted JSON) into the closed `EngineType` union. Apply
 * at every wire/persistence boundary; everything inside the service then keeps
 * strict typing.
 */
export function isEngineType(v: string | undefined): v is EngineType {
    return typeof v === 'string' && ENGINE_TYPE_SET.has(v)
}

const MODEL_EXPIRY_SET: ReadonlySet<string> = new Set(ModelExpiries)
/** Runtime type guard — validates a string is a known ModelExpiry. */
export function isModelExpiry(v: string | undefined): v is ModelExpiry {
    return v !== undefined && MODEL_EXPIRY_SET.has(v)
}

/**
 * No reported capability. This is what a node that sends no `capabilities`
 * object (an older peer) or a malformed one means: PAIR then offers nothing
 * rather than guessing from an engine name.
 */
export function noEngineCapabilities(): EngineCapabilityFlags {
    return {
        listModels: false,
        pullModel: false,
        deleteModel: false,
        deleteRestartsEngine: false,
        setPort: false,
        install: false,
        uninstall: false
    }
}

/**
 * Parse the `capabilities` object of an `nvpair-engine-manager` `EngineStatus`
 * (snake_case on the wire, from the local manager or from a peer's
 * `engine:remote-get-installed`). Every flag is read individually and defaults
 * to false, so a partial or absent object hides the actions it does not confirm.
 */
export function parseEngineCapabilities(value: JsonValue | undefined): EngineCapabilityFlags {
    const flag = (name: string): boolean => {
        if (!value || typeof value !== 'object' || Array.isArray(value)) return false
        const raw = value[name]
        return typeof raw === 'boolean' ? raw : false
    }
    return {
        listModels: flag('list_models'),
        pullModel: flag('pull_model'),
        deleteModel: flag('delete_model'),
        deleteRestartsEngine: flag('delete_restarts_engine'),
        setPort: flag('set_port'),
        install: flag('install'),
        uninstall: flag('uninstall')
    }
}

/** Shallow placeholder when the service has not yet reported this node/engine pair. */
export function emptyEngineStatus(nodeId: string, engineType: EngineType): EngineStatusData {
    return {
        engineType,
        nodeId,
        processStatus: 'initializing',
        capabilities: noEngineCapabilities(),
        enginePort: null,
        proxyPort: null
    }
}

/** Shallow placeholder when no model list has been pushed for this node/engine yet. */
export function emptyEngineModels(nodeId: string, engineType: EngineType): EngineModels {
    return { nodeId, engineType, models: [] }
}
