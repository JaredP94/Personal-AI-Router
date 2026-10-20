// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Engine manifest -- the single, immutable declaration of what an engine is.
 * Never broadcast over the wire; loaded locally from the engine registry.
 * Replaces BackendManifest, BackendCapabilities, BackendDisplayNames, BackendDefaultLinks.
 *
 * This is UI presentation metadata only. What an engine *supports* is not here:
 * `nvpair-engine-manager` reports it per node and platform on every
 * `EngineStatus` as `capabilities` (`EngineCapabilityFlags`), and the UI gates
 * model deletion, the delete confirmation and the editable engine port on those
 * reported flags.
 */
import type { SupportedPlatform } from '@/shared/types/platform'

export interface EngineHubConfig {
    /** Display label shown in the hub source selector (e.g. 'Ollama'). */
    label: string
    /** Public catalog URL that returns a JSON model list. */
    url: string
}

export interface EngineCaps {
    hasExpiry: boolean
    hasEject: boolean
    /** Platforms where PAIR can auto-install this engine. Empty = no auto-install. */
    hasInstall: SupportedPlatform[]
    /** Whether the engine requires a user-configured install path. */
    hasInstallPath: boolean
    /** When true, show an "open in browser" button that opens the proxy URL when running. */
    hasProxyWebUI: boolean
    /** When true, show a preferred node picker to pin proxy routing to a specific cluster node. */
    hasPreferredNode: boolean
    /** When true, surface a global error alert when the engine process crashes. */
    hasCrashAlert: boolean
    /** When true, show the model search/pull dropdown only while the engine process is running. */
    hasModelSearchOnlyWhenRunning: boolean
    /** When true, model operations work while the engine process is stopped (file-based weights). */
    modelOpsWhenStopped: boolean
    /**
     * If the engine has its own public model catalog (e.g. ollama.com), provide the
     * hub config here. Presence doubles as the truthy flag for the hub source selector.
     */
    engineHub?: EngineHubConfig
}
