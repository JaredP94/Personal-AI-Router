// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// @vitest-environment jsdom

import { act, createElement } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { EngineCapabilityFlags, ModelExpiry } from '@/shared/types/engines'
import ModelRow from '@/ui/components/ModelManager/ModelRow'
import { EngineCapabilities } from '@/ui/constants/engine-capabilities'
import type { ModelItem } from '@/ui/types/engine-info'

/**
 * The model action menu offers Delete only when the node that owns the model
 * reports a `delete_model` action. oMLX declares none, so its rows carry no
 * Delete however the row is built — the gate is the reported flag, not the
 * engine name.
 */

function reported(overrides: Partial<EngineCapabilityFlags> = {}): EngineCapabilityFlags {
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

let container: HTMLDivElement
let root: Root

/** The labels the action menu offers for this row, in render order. */
function menuItems(): HTMLElement[] {
    return Array.from(container.querySelectorAll('[data-testid="nv-dropdown-item"]'))
}

function menuLabels(): string[] {
    return menuItems().map(item => item.textContent ?? '')
}

function render(reportedForNode: EngineCapabilityFlags): void {
    act(() => {
        root.render(
            createElement(ModelRow, {
                model: MODEL,
                isRunning: true,
                capabilities: EngineCapabilities.omlx,
                reported: reportedForNode,
                displayName: (name: string) => name,
                onAction: vi.fn(),
                onExpiryChange: (_name: string, _value: ModelExpiry) => {}
            })
        )
    })
}

describe('ModelRow delete affordance', () => {
    beforeEach(() => {
        vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true)
        container = document.createElement('div')
        document.body.appendChild(container)
        root = createRoot(container)
    })

    afterEach(() => {
        act(() => root.unmount())
        container.remove()
        vi.unstubAllGlobals()
    })

    it('offers no Delete when the node reports no delete_model action', () => {
        render(reported({ pullModel: false, deleteModel: false, setPort: false }))
        expect(menuLabels()).toEqual(['Load', 'Eject'])
    })

    it('offers Delete once the node reports delete_model', () => {
        render(reported())
        expect(menuLabels()).toEqual(['Load', 'Eject', 'Delete'])
    })

    it('disables every action while a command for that model is in flight', () => {
        act(() => {
            root.render(
                createElement(ModelRow, {
                    model: MODEL,
                    isRunning: true,
                    capabilities: EngineCapabilities.ollama,
                    reported: reported(),
                    pendingAction: 'deleteModel',
                    displayName: (name: string) => name,
                    onAction: vi.fn(),
                    onExpiryChange: (_name: string, _value: ModelExpiry) => {}
                })
            )
        })
        const items = menuItems()
        expect(items).toHaveLength(3)
        expect(items.every(item => item.hasAttribute('disabled'))).toBe(true)
    })
})
