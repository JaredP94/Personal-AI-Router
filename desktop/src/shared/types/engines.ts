// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import {
    EngineOperationTypes,
    EngineProcessStatuses,
    EngineSources,
    EngineTypes,
    ModelExpiries,
    ModelItemStatuses
} from '@/shared/constants/engines'

/**
 * Canonical engine identifier — a closed literal union of every engine PAIR
 * ships. This codebase has no concept of "custom" or "user-registered"
 * engines: every engine is built in to the binary and listed in the
 * `EngineTypes` const tuple. Always prefer this type over `string` for
 * engine identifiers so the compiler flags typos like `'olllama'` or stale
 * literal values. Strings arriving from service bridge payloads or config JSON
 * must be narrowed via `isEngineType()` at the boundary.
 */
export type EngineType = (typeof EngineTypes)[number]

export type EngineSource = (typeof EngineSources)[number]

/**
 * What one engine can actually do on the node that reports it.
 *
 * `nvpair-engine-manager` derives these from the effective manifest for *that*
 * host's platform (and, for `set_port`/`uninstall`, from whether the engine is
 * an externally adopted install), so they are the only truth about support —
 * the desktop keeps no engine table of its own. A status that carries no
 * capability object (an older peer) is read as all-false, which hides rather
 * than promises an action.
 */
export type EngineCapabilityFlags = {
    /** The engine declares a `list_models` action on this platform. */
    listModels: boolean
    /** The engine declares a `pull_model` action on this platform. */
    pullModel: boolean
    /** The engine declares a `delete_model` action on this platform. */
    deleteModel: boolean
    /** `delete_model` is declared with `restart_after`, so a delete bounces the engine. */
    deleteRestartsEngine: boolean
    /** The engine's port can be moved persistently (`engine:set-port` would accept it). */
    setPort: boolean
    /** The effective manifest has a platform block for this host OS/arch. */
    install: boolean
    /** An uninstall is declared and the engine is not an externally adopted install. */
    uninstall: boolean
}

export interface EngineStatusData {
    engineType: EngineType
    nodeId: string
    processStatus: EngineProcessStatus
    /** Reported by the node that owns this engine. See {@link EngineCapabilityFlags}. */
    capabilities: EngineCapabilityFlags
    /**
     * Configured bind port for the engine HTTP server when installed (same value when stopped vs running).
     * Null when not installed, or for an engine with no server port.
     */
    enginePort: number | null
    /** PAIR's proxy port for this engine (e.g. Ollama 54301). Always set when the engine is installed. */
    proxyPort: number | null
    /**
     * Installed engine binary version reported by the owning node, when the
     * current status source exposes it. Undefined for nodes that have not
     * reported version data or engines that are not installed.
     */
    installedVersion?: string
}

export type ModelItemStatus = (typeof ModelItemStatuses)[number]

export type ModelExpiry = (typeof ModelExpiries)[number]

/** nodeId → engineType → status (sparse: only types reported by the service). */
export type EngineStatusByNode = Map<string, Map<EngineType, EngineStatusData>>

/**
 * Engine status domain -- broadcast per engine per node when process state changes.
 * Replaces the process/port/autoStart portion of the monolithic BackendInfo.
 */

export type EngineProcessStatus = (typeof EngineProcessStatuses)[number]

export type EngineOperationType = (typeof EngineOperationTypes)[number]

export interface EngineProgress {
    engineType: EngineType
    nodeId: string
    nodeName: string
    operation: EngineOperationType
    /** Set for model operations (pull, load, unload, delete). */
    model?: string
    /** 'started' | 'downloading' | 'complete' | 'error' | engine-specific status string. */
    status: string
    percent?: number
    completed?: number
    total?: number
    error?: string
}

/**
 * Available engine-update signal for one engine on one node. The backend
 * reports it per node; PAIR receives it through `engines:state-changed` and
 * the renderer's `engine-update-available.store`, so the Edit Node UI can
 * surface an Update affordance for any node — local or remote — the user can
 * reach in the cluster.
 */
export interface EngineUpdateAvailable {
    nodeId: string
    engineType: EngineType
    currentVersion: string
    latestVersion: string
    releaseUrl: string
    installType: 'managed' | 'external'
}

export interface EngineModels {
    engineType: EngineType
    nodeId: string
    models: ModelItem[]
}

export interface ModelItem {
    name: string
    size: number
    downloaded: boolean
    status: ModelItemStatus
    parameterSize: string
    quantization: string
    family: string
    digest: string
    /** VRAM consumed by the model when loaded (bytes). */
    sizeVram: number | null
    /** Timestamp when the engine will auto-unload this model. */
    expiresAt: string | null
    /** User-configured keep_alive duration sent when loading the model. */
    expiry: ModelExpiry
    /**
     * Original key used to pull/download this model (e.g. HuggingFace `author/repo/file.gguf`).
     * Stored alongside the model so sync can reproduce the pull on other nodes.
     * Absent for models whose `name` is already a valid pull key (e.g. Ollama tags).
     */
    pullKey?: string
    /**
     * Engine-reported model capabilities (lowercase). Known values include
     * `'vision'`, `'tools'`, `'embedding'`, `'completion'`, `'thinking'`.
     * Empty array when the engine does not expose capability metadata for
     * this model. Consumers that don't care can ignore; the chat UI uses
     * `'vision'` to gate image attachment affordances.
     */
    capabilities: string[]
}
