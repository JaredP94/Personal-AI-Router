// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// @vitest-environment jsdom

import { act, createElement } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ErrorModal } from '@/ui/components/ErrorModal'
import { useErrorsStore } from '@/ui/stores/errors.store'
import { useEngineStatusStore } from '@/ui/stores/engine-status.store'
import { noEngineCapabilities } from '@/shared/utils/engines'
import type { EngineCapabilityFlags, EngineType } from '@/shared/types/engines'

let container: HTMLDivElement
let root: Root
const pullModel = vi.fn()
const install = vi.fn()
const uninstall = vi.fn()
const toggle = vi.fn()

function report(nodeId: string, engineType: EngineType, capabilities: EngineCapabilityFlags): void {
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
                    enginePort: 1234,
                    proxyPort: 54301,
                    capabilities
                }
            ]
        ])
    )
    act(() => useEngineStatusStore.setState({ statusByNode }))
}

function render(operation: string, engineType: EngineType = 'ollama', nodeId = 'peer'): void {
    act(() => {
        useErrorsStore.setState({
            errors: [
                {
                    id: 'retained-failure',
                    message: 'Failed operation',
                    timestamp: 1,
                    engineType,
                    nodeId,
                    operation,
                    modelName: 'demo',
                    action: 'retry'
                }
            ]
        })
        root.render(createElement(ErrorModal))
    })
}

function retry(): HTMLButtonElement | undefined {
    return Array.from(document.querySelectorAll('button')).find(
        button => button.textContent === 'Retry'
    )
}

describe('ErrorModal retry capability gates', () => {
    beforeEach(() => {
        vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true)
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
        vi.stubGlobal('pairApi', {
            engines: { pullModel, install, uninstall, toggle },
            errors: { clear: vi.fn() }
        })
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
        useErrorsStore.setState({ errors: [] })
        useEngineStatusStore.setState({ statusByNode: new Map() })
        vi.unstubAllGlobals()
    })

    it('never offers a retained oMLX pull retry locally or on a peer', () => {
        for (const nodeId of ['local', 'peer']) {
            report(nodeId, 'omlx', { ...noEngineCapabilities(), listModels: true, install: true })
            render('pull', 'omlx', nodeId)
            expect(retry()).toBeUndefined()
        }
        expect(pullModel).not.toHaveBeenCalled()
    })

    it('does not borrow local pull support for a peer with missing capabilities', () => {
        report('local', 'ollama', { ...noEngineCapabilities(), pullModel: true })
        render('pull')
        expect(retry()).toBeUndefined()
        expect(pullModel).not.toHaveBeenCalled()
    })

    for (const operation of ['pull', 'install', 'uninstall']) {
        it(`requires the owning node's ${operation} capability and updates a retained error live`, () => {
            report('peer', 'ollama', noEngineCapabilities())
            render(operation)
            expect(retry()).toBeUndefined()
            report('peer', 'ollama', {
                ...noEngineCapabilities(),
                pullModel: true,
                install: true,
                uninstall: true
            })
            expect(retry()).toBeDefined()
            report('peer', 'ollama', noEngineCapabilities())
            expect(retry()).toBeUndefined()
            report('peer', 'ollama', {
                ...noEngineCapabilities(),
                pullModel: true,
                install: true,
                uninstall: true
            })
            act(() => retry()?.click())
            if (operation === 'pull')
                expect(pullModel).toHaveBeenCalledExactlyOnceWith('ollama', 'peer', 'demo')
            if (operation === 'install')
                expect(install).toHaveBeenCalledExactlyOnceWith('ollama', 'peer')
            if (operation === 'uninstall')
                expect(uninstall).toHaveBeenCalledExactlyOnceWith('ollama', 'peer')
        })
    }
})
