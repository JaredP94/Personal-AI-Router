// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'
import { parseEngineCapabilities } from '@/shared/utils/engines'
import type { EngineCapabilityFlags } from '@/shared/types/engines'
import { modelActionItems } from '@/ui/utils/engine-capability-gates'
import { EngineCapabilities } from '@/ui/constants/engine-capabilities'
import type { EngineCaps } from '@/ui/types/engine-manifest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'

/**
 * The engine manager owns capability policy: every `EngineStatus` carries the
 * flags that hold on the *owning* node's platform, and the UI gates Delete,
 * the delete confirmation and the editable server port on those reported flags.
 * Nothing in the desktop tree keeps an engine table of what is supported, so
 * oMLX (no `delete_model`, no `{port}` in its start command) offers neither.
 */

/** The wire object `nvpair-engine-manager` puts on every `EngineStatus`. */
function wireCapabilities(
    overrides: Partial<Record<string, boolean>> = {}
): Record<string, boolean> {
    return {
        list_models: true,
        pull_model: true,
        delete_model: true,
        delete_restarts_engine: false,
        set_port: true,
        install: true,
        uninstall: true,
        ...overrides
    }
}

function flags(overrides: Partial<EngineCapabilityFlags> = {}): EngineCapabilityFlags {
    return {
        listModels: true,
        pullModel: true,
        deleteModel: true,
        deleteRestartsEngine: false,
        setPort: true,
        install: true,
        uninstall: true,
        ...overrides
    }
}

const OMLX_STATIC_CAPS: EngineCaps = EngineCapabilities.omlx
const OLLAMA_STATIC_CAPS: EngineCaps = EngineCapabilities.ollama

describe('capability parsing', () => {
    it('maps every reported flag onto the camelCase client type', () => {
        expect(
            parseEngineCapabilities(
                wireCapabilities({
                    list_models: true,
                    pull_model: false,
                    delete_model: true,
                    delete_restarts_engine: true,
                    set_port: false,
                    install: true,
                    uninstall: false
                })
            )
        ).toEqual({
            listModels: true,
            pullModel: false,
            deleteModel: true,
            deleteRestartsEngine: true,
            setPort: false,
            install: true,
            uninstall: false
        })
    })

    it('accepts true for all seven flags without dropping one', () => {
        expect(parseEngineCapabilities(wireCapabilities({ delete_restarts_engine: true }))).toEqual(
            {
                listModels: true,
                pullModel: true,
                deleteModel: true,
                deleteRestartsEngine: true,
                setPort: true,
                install: true,
                uninstall: true
            }
        )
    })

    it('treats a status without a capabilities object as all-false', () => {
        const expected: EngineCapabilityFlags = {
            listModels: false,
            pullModel: false,
            deleteModel: false,
            deleteRestartsEngine: false,
            setPort: false,
            install: false,
            uninstall: false
        }
        // An older peer that predates the contract sends no object at all.
        expect(parseEngineCapabilities(undefined)).toEqual(expected)
        // A malformed one is treated the same way, never trusted.
        for (const value of [null, false, 1, 'nope', [], { delete_model: 'yes' }]) {
            expect(parseEngineCapabilities(value)).toEqual(expected)
        }
        expect(
            parseEngineCapabilities({
                list_models: false,
                pull_model: false,
                delete_model: false,
                delete_restarts_engine: false,
                set_port: false,
                install: false,
                uninstall: false
            })
        ).toEqual(expected)
    })

    it('defaults each absent flag to false and keeps the reported ones', () => {
        expect(
            parseEngineCapabilities({ delete_model: true, set_port: true })
        ).toEqual<EngineCapabilityFlags>({
            listModels: false,
            pullModel: false,
            deleteModel: true,
            deleteRestartsEngine: false,
            setPort: true,
            install: false,
            uninstall: false
        })
    })
})

describe('model actions are gated on the node that owns the row', () => {
    it('offers no Delete for an engine that reports no delete_model action (oMLX)', () => {
        const items = modelActionItems({
            capabilities: OMLX_STATIC_CAPS,
            reported: flags({ pullModel: false, deleteModel: false, setPort: false }),
            isRunning: true,
            isLoaded: false,
            downloaded: true,
            busy: false
        })
        expect(items.map(item => item.id)).not.toContain('delete')
    })

    it('offers Delete once the node reports delete_model', () => {
        const items = modelActionItems({
            capabilities: OLLAMA_STATIC_CAPS,
            reported: flags(),
            isRunning: true,
            isLoaded: false,
            downloaded: true,
            busy: false
        })
        expect(items.find(item => item.id === 'delete')).toMatchObject({
            danger: true,
            disabled: false
        })
    })

    it('locks every action while a command for that model is in flight', () => {
        const items = modelActionItems({
            capabilities: OLLAMA_STATIC_CAPS,
            reported: flags(),
            isRunning: true,
            isLoaded: false,
            downloaded: true,
            busy: true
        })
        expect(items.every(item => item.disabled === true)).toBe(true)
    })
})

describe('the static UI table keeps no engine capability policy', () => {
    it('exposes no delete/restart/port capability keys for any engine', () => {
        const retired = ['hasDeleteModel', 'restartsOnModelDelete', 'hasEnginePort']
        for (const [engine, caps] of Object.entries(EngineCapabilities)) {
            for (const key of retired) {
                expect(
                    Object.keys(caps).includes(key),
                    `${engine} still hardcodes ${key}; the node reports it instead`
                ).toBe(false)
            }
        }
    })
})

describe("engine status carries the owning node's reported capabilities", () => {
    it('carries them for the local node and for a remote peer, separately', () => {
        const state = getModularBridgeState()
        state.setSelfId('gates-local')
        state.handleNotification({
            source: 'proxy',
            method: 'node/discovered',
            params: { id: 'gates-remote', host: 'gates-remote', port: 54301 }
        })
        state.applyEngineManagerStatus({
            engine: 'ollama',
            installed: true,
            running: true,
            healthy: true,
            port: 11434,
            capabilities: wireCapabilities()
        })
        // A peer's effective manifest can support different actions for the
        // very same engine. Never substitute the local node's flags.
        state.applyRemoteEngineFacts('gates-remote', {
            engines: [
                {
                    engine: 'ollama',
                    installed: true,
                    running: true,
                    healthy: true,
                    port: 11434,
                    capabilities: wireCapabilities({
                        pull_model: false,
                        delete_model: false,
                        set_port: false,
                        uninstall: false
                    })
                }
            ]
        })

        const statuses = state.getEngineInitialState().statuses
        const local = statuses.find(s => s.nodeId === 'gates-local' && s.engineType === 'ollama')
        const remote = statuses.find(s => s.nodeId === 'gates-remote' && s.engineType === 'ollama')

        expect(local?.capabilities).toEqual(flags())
        // The same engine, two nodes: the remote row must read the peer's flags.
        expect(remote?.capabilities).toEqual(
            flags({ pullModel: false, deleteModel: false, setPort: false, uninstall: false })
        )
    })

    it('preserves flags during local and remote transitions and replaces them on lifecycle results', () => {
        const state = getModularBridgeState()
        state.setSelfId('gates-lifecycle')
        state.handleNotification({
            source: 'proxy',
            method: 'node/discovered',
            params: { id: 'gates-remote', host: 'gates-remote', port: 54301 }
        })
        state.applyEngineManagerStatus({
            engine: 'lmstudio',
            installed: true,
            running: true,
            port: 1235,
            capabilities: wireCapabilities({ delete_restarts_engine: true })
        })
        state.beginLocalEngineOp('lm-studio', 'stopping')
        try {
            const local = state
                .getEngineInitialState()
                .statuses.find(s => s.nodeId === 'gates-lifecycle' && s.engineType === 'lm-studio')
            expect(local?.processStatus).toBe('stopping')
            expect(local?.capabilities).toEqual(flags({ deleteRestartsEngine: true }))
        } finally {
            state.clearPendingEngineOp('lm-studio')
        }
        state.applyRemoteEngineStatusResult('gates-remote', 'ollama', {
            engine: 'ollama',
            installed: true,
            running: false,
            port: 11434,
            capabilities: wireCapabilities({ set_port: false })
        })
        expect(
            state
                .getEngineInitialState()
                .statuses.find(s => s.nodeId === 'gates-remote' && s.engineType === 'ollama')
                ?.capabilities
        ).toEqual(flags({ setPort: false }))
        state.beginRemoteEngineOp('gates-remote', 'ollama', 'starting')
        try {
            const remote = state
                .getEngineInitialState()
                .statuses.find(s => s.nodeId === 'gates-remote' && s.engineType === 'ollama')
            expect(remote?.processStatus).toBe('starting')
            expect(remote?.capabilities).toEqual(flags({ setPort: false }))
        } finally {
            state.clearPendingRemoteEngineOp('gates-remote', 'ollama')
        }
        // A later status without flags revokes capabilities rather than retaining
        // stale permission from the preceding report.
        state.applyEngineManagerStatus({ engine: 'lmstudio', installed: true, running: false })
        expect(
            state
                .getEngineInitialState()
                .statuses.find(s => s.nodeId === 'gates-lifecycle' && s.engineType === 'lm-studio')
                ?.capabilities
        ).toEqual({
            listModels: false,
            pullModel: false,
            deleteModel: false,
            deleteRestartsEngine: false,
            setPort: false,
            install: false,
            uninstall: false
        })
    })

    it('treats a peer that sends no capabilities object as all-false', () => {
        const state = getModularBridgeState()
        state.setSelfId('gates-local-2')
        state.handleNotification({
            source: 'proxy',
            method: 'node/discovered',
            params: { id: 'gates-remote-2', host: 'gates-remote-2', port: 54301 }
        })
        state.applyRemoteEngineFacts('gates-remote-2', {
            engines: [{ engine: 'ollama', installed: true, running: false, port: 11434 }]
        })

        const remote = state
            .getEngineInitialState()
            .statuses.find(s => s.nodeId === 'gates-remote-2' && s.engineType === 'ollama')
        expect(remote?.capabilities).toEqual({
            listModels: false,
            pullModel: false,
            deleteModel: false,
            deleteRestartsEngine: false,
            setPort: false,
            install: false,
            uninstall: false
        })
    })
})
