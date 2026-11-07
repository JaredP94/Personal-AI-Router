// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'
vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))
import { getModularBridgeState, isProxyEngine } from '@/electron/service-bridge/modular-state'
import { parseEngineSettings } from '@/electron/service-bridge/engine-settings'

const capabilities = {
    list_models: true,
    pull_model: false,
    delete_model: false,
    delete_restarts_engine: false,
    set_port: true,
    install: true,
    uninstall: true
}

describe('llama.cpp routing projection', () => {
    it('shows local engine facts and the independently reported proxy port', () => {
        const state = getModularBridgeState()
        state.setSelfId('llama-self')
        state.handleNotification({
            source: 'llamacpp-proxy',
            method: 'ready',
            params: { port: 1237 }
        })
        for (const running of [false, true]) {
            state.applyEngineManagerStatus({
                engine: 'llamacpp',
                installed: true,
                running,
                port: 8082,
                capabilities
            })
            const status = state
                .getEngineInitialState()
                .statuses.find(s => s.nodeId === 'llama-self' && s.engineType === 'llamacpp')
            expect(status).toMatchObject({
                processStatus: running ? 'running' : 'stopped',
                enginePort: 8082,
                proxyPort: 1237,
                capabilities: { install: true, listModels: true, pullModel: false }
            })
        }
        expect(isProxyEngine('llamacpp')).toBe(true)
    })

    it('shows remote lifecycle facts without attributing a routed endpoint', () => {
        const state = getModularBridgeState()
        state.setSelfId('llama-self')
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        identityVerification: 'verified',
                        peerRecordId: 'llama-proof',
                        hostUuid: 'llama-peer',
                        name: 'Peer',
                        ipAddress: '192.0.2.80',
                        port: 14318,
                        trusted: true,
                        clustered: true
                    }
                ]
            }
        })
        state.applyRemoteEngineFacts('llama-peer', {
            engines: [
                {
                    engine: 'llamacpp',
                    installed: true,
                    running: true,
                    healthy: true,
                    port: 8082,
                    capabilities
                }
            ]
        })
        expect(
            state
                .getEngineInitialState()
                .statuses.find(s => s.nodeId === 'llama-peer' && s.engineType === 'llamacpp')
        ).toMatchObject({ processStatus: 'running', enginePort: 8082, proxyPort: null })
        expect(
            state
                .getEngineInitialState()
                .models.filter(s => s.nodeId === 'llama-peer' && s.engineType === 'llamacpp')
        ).toMatchObject([{ engineType: 'llamacpp', nodeId: 'llama-peer', models: [] }])
        expect(state.isRemoteEngineRunning('llama-peer', 'llamacpp')).toBe(true)
    })

    it('accepts a routed proxy-port settings snapshot without adding load profiles', () => {
        expect(
            parseEngineSettings({
                nodeId: 'llama-self',
                engine: 'llamacpp',
                phase: 'idle',
                epoch: 'fixture',
                sequence: 1,
                revision: 1,
                appliedRevision: 1,
                settings: { serverPort: 8082, proxyPort: 1237, launchText: '' },
                effectiveServerPort: 8082,
                effectiveProxyPort: 1237,
                running: false,
                adopted: false,
                editable: true,
                format: 'pair-arguments-v1'
            })
        ).toMatchObject({ engine: 'llamacpp', settings: { proxyPort: 1237 } })
    })
})
