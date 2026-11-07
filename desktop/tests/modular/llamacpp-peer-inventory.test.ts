// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'
vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))
import { getModularBridgeState } from '@/electron/service-bridge/modular-state'

describe('llama.cpp verified peer installed inventory', () => {
    it('projects owned and unrecorded cache rows while stopped, not served names', () => {
        const state = getModularBridgeState()
        state.setSelfId('cache-self')
        const node = {
            hostUuid: 'cache-peer',
            peerRecordId: 'cache-proof',
            identityVerification: 'verified',
            name: 'Peer',
            ipAddress: '192.0.2.90',
            port: 14318,
            trusted: true,
            modelsByEngine: { llamacpp: ['served-only'] },
            downloadedByEngine: {
                llamacpp: [
                    {
                        id: 'publisher/model:Q4_K_M',
                        file: 'owned.gguf',
                        size: 42,
                        sha256: 'verified',
                        owned: true
                    },
                    { id: 'manual.gguf', file: 'manual.gguf', size: 24, sha256: '', owned: false }
                ]
            }
        }
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: { nodes: [node] }
        })
        state.applyRemoteEngineFacts('cache-peer', {
            engines: [
                {
                    engine: 'llamacpp',
                    installed: true,
                    running: false,
                    capabilities: { list_models: true, delete_model: true }
                }
            ]
        })
        const models = () =>
            state
                .getEngineInitialState()
                .models.find(row => row.nodeId === 'cache-peer' && row.engineType === 'llamacpp')
                ?.models
        expect(state.getPeerDownloadedModels('cache-peer')[0]).toMatchObject({
            file: 'owned.gguf',
            size: 42
        })
        expect(models()).toMatchObject([
            { name: 'publisher/model:Q4_K_M', size: 42, digest: 'verified', downloaded: true },
            { name: 'manual.gguf', size: 24, digest: '', downloaded: true }
        ])
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: { nodes: [{ ...node, downloadedByEngine: { llamacpp: [] } }] }
        })
        expect(models()).toEqual([])
    })

    it('never projects cache inventory from an unverified preview', () => {
        const state = getModularBridgeState()
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: 'preview:cache',
                        peerRecordId: '',
                        identityVerification: 'unverified',
                        name: 'Preview',
                        downloadedByEngine: {
                            llamacpp: [
                                {
                                    id: 'forged',
                                    file: 'forged.gguf',
                                    size: 24,
                                    sha256: 'forged',
                                    owned: true
                                }
                            ]
                        }
                    }
                ]
            }
        })
        expect(
            state
                .getEngineInitialState()
                .models.some(row => row.models.some(model => model.name === 'forged'))
        ).toBe(false)
        expect(() => state.getPeerDownloadedModels('preview:cache')).toThrow('verified peer')
    })
})
