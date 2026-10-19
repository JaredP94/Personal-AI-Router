// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// @vitest-environment jsdom

import { act, createElement } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { EngineCapabilityFlags, EngineType } from '@/shared/types/engines'
import type { BackendInfo, ModelItem } from '@/ui/types/engine-info'
import { ModelManager } from '@/ui/components/ModelManager/ModelManager'
import { useEngineStatusStore } from '@/ui/stores/engine-status.store'

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

function render(nodeId = 'local', engineType: EngineType = 'lm-studio'): void {
    const backend: BackendInfo = {
        type: engineType,
        displayName: engineType === 'lm-studio' ? 'LM Studio' : 'oMLX',
        source: 'installed',
        processStatus: 'running',
        port: 1234,
        proxyPort: 54301,
        models: [MODEL]
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
        vi.stubGlobal('pairApi', { engines: { deleteModel, pullModel } })
        useEngineStatusStore.setState({ statusByNode: new Map() })
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
        vi.unstubAllGlobals()
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
