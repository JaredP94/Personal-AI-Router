// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// @vitest-environment jsdom

import { act, createElement } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { EngineCapabilityFlags, EngineType, EngineProcessStatus } from '@/shared/types/engines'
import type { BackendInfo, ModelItem } from '@/ui/types/engine-info'
import { ModelManager } from '@/ui/components/ModelManager/ModelManager'
import { useEngineProgressStore } from '@/ui/stores/engine-progress.store'
import type { EngineProgress } from '@/shared/types/engines'
import { useEngineModelsStore } from '@/ui/stores/engine-models.store'
import { useEngineStatusStore } from '@/ui/stores/engine-status.store'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useNodesStore } from '@/ui/stores/nodes.store'

const MODEL: ModelItem = {
    name: 'publisher/demo',
    size: 1,
    downloaded: true,
    status: 'idle',
    parameterSize: '',
    quantization: '',
    family: '',
    digest: '',
    sizeVram: null,
    expiresAt: null,
    expiry: '-1',
    capabilities: []
}

const supported: EngineCapabilityFlags = {
    listModels: true,
    pullModel: true,
    deleteModel: true,
    deleteRestartsEngine: true,
    setPort: true,
    install: true,
    uninstall: true
}

let container: HTMLDivElement
let root: Root
const deleteModel = vi.fn()
const pullModel = vi.fn()
const downloadedModels = vi.fn(async () => [
    { id: MODEL.name, file: 'cache.gguf', size: 42, sha256: 'verified' }
])

function report(nodeId: string, engineType: EngineType, flags: EngineCapabilityFlags): void {
    const statusByNode = new Map(useEngineStatusStore.getState().statusByNode)
    statusByNode.set(
        nodeId,
        new Map([
            [
                engineType,
                {
                    nodeId,
                    engineType,
                    processStatus: 'running',
                    capabilities: flags,
                    enginePort: 1234,
                    proxyPort: 54301
                }
            ]
        ])
    )
    act(() => useEngineStatusStore.setState({ statusByNode }))
}

function render(
    nodeId = 'local',
    engineType: EngineType = 'lm-studio',
    downloaded = true,
    processStatus: EngineProcessStatus = 'running',
    digest = '',
    models: ModelItem[] = [{ ...MODEL, downloaded, digest }]
): void {
    const backend: BackendInfo = {
        type: engineType,
        displayName: engineType === 'lm-studio' ? 'LM Studio' : 'oMLX',
        source: 'installed',
        processStatus,
        port: 1234,
        proxyPort: 54301,
        models
    }
    act(() => root.render(createElement(ModelManager, { backend, nodeId })))
}

function click(label: string): void {
    const target = Array.from(
        document.querySelectorAll<HTMLElement>('button, [data-testid="nv-dropdown-item"]')
    ).find(element => element.textContent === label)
    expect(target, `missing control ${label}`).toBeDefined()
    act(() => target?.click())
}

function labels(): string[] {
    return Array.from(document.querySelectorAll('button, [data-testid="nv-dropdown-item"]')).map(
        element => element.textContent ?? ''
    )
}

describe('ModelManager reported capability gates', () => {
    beforeEach(() => {
        vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true)
        // jsdom has no native dialog API; keep the real ConfirmModal and supply
        // only the browser operations used by Foundations' dialog wrapper.
        Object.defineProperty(HTMLDialogElement.prototype, 'showModal', {
            configurable: true,
            value(this: HTMLDialogElement) {
                this.open = true
            }
        })
        Object.defineProperty(HTMLDialogElement.prototype, 'close', {
            configurable: true,
            value(this: HTMLDialogElement) {
                this.open = false
                this.dispatchEvent(new Event('close'))
            }
        })
        vi.stubGlobal('pairApi', { engines: { deleteModel, pullModel, downloadedModels } })
        useEngineStatusStore.setState({ statusByNode: new Map() })
        useConnectionStore.setState({ connected: true, selfId: 'local' })
        container = document.createElement('div')
        document.body.appendChild(container)
        root = createRoot(container)
    })

    afterEach(() => {
        act(() => root.unmount())
        container.remove()
        Reflect.deleteProperty(HTMLDialogElement.prototype, 'showModal')
        Reflect.deleteProperty(HTMLDialogElement.prototype, 'close')
        useEngineStatusStore.setState({ statusByNode: new Map() })
        useEngineProgressStore.setState({ progress: new Map() })
        useEngineModelsStore.setState({ models: new Map() })
        useConnectionStore.setState({ connected: false, selfId: null })
        useNodesStore.setState({ nodes: new Map() })
        vi.unstubAllGlobals()
    })

    it('shows a peer cache error calmly on llama.cpp only', () => {
        report('peer', 'llamacpp', supported)
        act(() =>
            useEngineModelsStore.setState({
                models: new Map([
                    [
                        'peer:llamacpp',
                        {
                            nodeId: 'peer',
                            engineType: 'llamacpp',
                            models: [],
                            downloadError: "Couldn't read llama.cpp's downloaded models"
                        }
                    ]
                ])
            })
        )
        render('peer', 'llamacpp', true, 'stopped', '', [])
        expect(container.textContent).toContain("Couldn't read llama.cpp's downloaded models")
        expect(container.textContent).not.toContain('No model/weight files downloaded')
        report('peer', 'lm-studio', supported)
        render('peer', 'lm-studio')
        expect(container.textContent).not.toContain("Couldn't read llama.cpp's downloaded models")
        expect(container.textContent).toContain('demo')
        report('peer', 'llamacpp', supported)
        render('peer', 'llamacpp', true, 'stopped', '', [])
        act(() => useEngineModelsStore.setState({ models: new Map() }))
        expect(container.textContent).not.toContain("Couldn't read llama.cpp's downloaded models")
        expect(container.textContent).toContain('No model/weight files downloaded')
    })

    it('refreshes the stopped llama.cpp installed list only after deletion finishes', async () => {
        let finish = (): void => {}
        let removed = false
        downloadedModels.mockImplementation(async () =>
            removed ? [] : [{ id: MODEL.name, file: 'cache.gguf', size: 42, sha256: 'verified' }]
        )
        deleteModel.mockImplementation(
            () =>
                new Promise<void>(resolve => {
                    finish = () => {
                        removed = true
                        resolve()
                    }
                })
        )
        report('local', 'llamacpp', { ...supported, deleteRestartsEngine: false })
        await act(async () => render('local', 'llamacpp', true, 'stopped'))
        expect(container.textContent).toContain(MODEL.name)
        click('Delete')
        await act(async () => {
            await Promise.resolve()
        })
        expect(container.textContent).toContain(MODEL.name)
        await act(async () => finish())
        expect(container.textContent).not.toContain(MODEL.name)
        expect(container.textContent).toContain('No model/weight files downloaded')
    })

    it('offers owned peer llama.cpp deletion and explains the running gate', async () => {
        useNodesStore.setState({
            nodes: new Map([
                [
                    'peer',
                    {
                        id: 'peer',
                        hostUuid: 'peer',
                        peerRecordId: 'proof',
                        identityVerification: 'verified',
                        name: 'Peer',
                        ipAddress: '192.0.2.10',
                        port: 14318,
                        allIpAddresses: [],
                        status: 'active',
                        os: 'Linux',
                        topology: {
                            gpus: [],
                            cpu: { model: '', cores: 0, threads: 0 },
                            ram: 0,
                            storage: []
                        }
                    }
                ]
            ])
        })
        deleteModel.mockResolvedValue(undefined)
        report('peer', 'llamacpp', { ...supported, deleteRestartsEngine: false })
        await act(async () => render('peer', 'llamacpp', true, 'stopped', 'verified'))
        expect(container.textContent).toContain(MODEL.name)
        expect(container.textContent).not.toContain('Only served')
        await act(async () => click('Delete'))
        expect(deleteModel).toHaveBeenCalledWith('llamacpp', 'peer', MODEL.name)
        render('peer', 'llamacpp', true, 'running', 'verified')
        const gate = Array.from(container.querySelectorAll<HTMLButtonElement>('button')).find(
            button => button.textContent === 'Delete (stop engine first)'
        )
        expect(gate?.disabled).toBe(true)
        render('peer', 'llamacpp', true, 'stopped')
        expect(labels()).not.toContain('Delete')
    })

    it('includes Load options in downloaded local LM Studio model rows only', () => {
        report('local', 'lm-studio', supported)
        render()
        expect(container.textContent).toContain('Load options')
        render('local', 'lm-studio', false)
        expect(container.textContent).not.toContain('Load options')
        report('local', 'omlx', supported)
        render('local', 'omlx')
        expect(container.textContent).not.toContain('Load options')
    })

    it('includes Load options for a verified peer model, but not an unverified preview', () => {
        useNodesStore.setState({
            nodes: new Map([
                [
                    'peer',
                    {
                        id: 'peer',
                        hostUuid: 'peer',
                        peerRecordId: 'proof',
                        identityVerification: 'verified',
                        name: 'Peer',
                        ipAddress: '192.0.2.10',
                        port: 14318,
                        allIpAddresses: [],
                        status: 'active',
                        os: 'Linux',
                        topology: {
                            gpus: [],
                            cpu: { model: '', cores: 0, threads: 0 },
                            ram: 0,
                            storage: []
                        }
                    }
                ]
            ])
        })
        report('peer', 'lm-studio', supported)
        render('peer')
        expect(container.textContent).toContain('Load options')
        report('preview:claim', 'lm-studio', supported)
        render('preview:claim')
        expect(container.textContent).not.toContain('Load options')
    })

    it('offers Cancel when registration changes only canCancel, without a percent tick', () => {
        const progress: EngineProgress = {
            nodeId: 'local',
            nodeName: 'local',
            engineType: 'lm-studio',
            operation: 'pull',
            model: 'new-model',
            opId: 'registered',
            status: 'pulling',
            canCancel: false
        }
        const publish = (canCancel: boolean): void => {
            act(() =>
                useEngineProgressStore.setState({
                    progress: new Map([
                        ['local:lm-studio:pull:new-model', { ...progress, canCancel }]
                    ])
                })
            )
        }
        publish(false)
        report('local', 'lm-studio', supported)
        render()
        expect(labels()).not.toContain('Cancel')
        publish(true)
        expect(labels()).toContain('Cancel')
        act(() => useEngineProgressStore.setState({ progress: new Map() }))
    })

    it('requires confirmation before LM Studio delete and sends nothing on cancel', () => {
        report('local', 'lm-studio', supported)
        render()
        click('Delete')
        expect(deleteModel).not.toHaveBeenCalled()
        expect(document.body.textContent).toContain('requires LM Studio to be restarted')
        click('Cancel')
        expect(deleteModel).not.toHaveBeenCalled()
        click('Delete')
        click('Delete and restart')
        expect(deleteModel).toHaveBeenCalledExactlyOnceWith('lm-studio', 'local', MODEL.name)
    })

    it('confirms the original node even if the displayed node changes while the prompt is open', () => {
        report('local', 'lm-studio', supported)
        report('peer', 'lm-studio', supported)
        render('local')
        click('Delete')
        render('peer')
        click('Delete and restart')
        expect(deleteModel).toHaveBeenCalledExactlyOnceWith('lm-studio', 'local', MODEL.name)
    })

    it('uses the peer flags, not the local flags for the same engine', () => {
        report('local', 'lm-studio', supported)
        report('peer', 'lm-studio', { ...supported, deleteRestartsEngine: false })
        render('peer')
        click('Delete')
        expect(deleteModel).toHaveBeenCalledExactlyOnceWith('lm-studio', 'peer', MODEL.name)
        expect(labels()).not.toContain('Delete and restart')
    })

    it('does not offer oMLX pull or delete locally or on a peer', () => {
        for (const nodeId of ['local', 'peer']) {
            report(nodeId, 'omlx', { ...supported, pullModel: false, deleteModel: false })
            render(nodeId, 'omlx')
            expect(labels()).not.toContain('Add model')
            expect(labels()).not.toContain('Delete')
        }
        expect(deleteModel).not.toHaveBeenCalled()
        expect(pullModel).not.toHaveBeenCalled()
    })

    it('shows Add model only for the owning node that reports pull_model', () => {
        report('local', 'lm-studio', supported)
        report('peer', 'lm-studio', { ...supported, pullModel: false, deleteModel: false })
        render('peer')
        expect(labels()).not.toContain('Add model')
        expect(labels()).not.toContain('Delete')
        render('local')
        expect(labels()).toContain('Add model')
    })

    it('does not show an inventory that the node cannot list', () => {
        report('local', 'lm-studio', { ...supported, listModels: false })
        render()
        expect(labels()).not.toContain('Load')
        expect(labels()).toContain('Add model')
    })

    it('renders an unreported node conservatively without unstable snapshot selectors', () => {
        render('older-peer')
        expect(labels()).not.toContain('Delete')
        expect(labels()).not.toContain('Add model')
    })
})
