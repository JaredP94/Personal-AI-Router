// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// @vitest-environment jsdom

import { act, createElement } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { EngineCapabilityFlags } from '@/shared/types/engines'
import { PortsSection } from '@/ui/components/BackendRow/PortsSection'

/**
 * The engine's own server port is editable only when the owning node reports
 * `set_port` — oMLX's bundled start command takes no port at all, so its row
 * must not offer the field. The value under test is the reported flag, not a
 * table of what an engine name usually supports.
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

let container: HTMLDivElement
let root: Root

function render(reported: EngineCapabilityFlags): void {
    act(() => {
        root.render(
            createElement(PortsSection, {
                edit: { serverPort: '11434', proxyPort: '54301' },
                portsChanged: false,
                anyLoading: false,
                isLocalNode: true,
                canSetPort: reported.setPort,
                onApplyPorts: vi.fn(),
                onServerChange: vi.fn(),
                onProxyChange: vi.fn()
            })
        )
    })
}

describe('PortsSection server-port field', () => {
    /** The editable values, in render order (the port inputs, not the buttons). */
    function inputValues(): string[] {
        return Array.from(container.querySelectorAll('input')).map(input => input.value)
    }

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

    it('shows the server port when the node reports set_port', () => {
        render(reported())
        expect(container.textContent).toContain('Server')
        expect(inputValues()).toContain('11434')
    })

    it('hides the server port when the node reports no set_port, keeping the proxy port', () => {
        render(reported({ setPort: false }))
        expect(container.textContent).not.toContain('Server')
        expect(container.textContent).toContain('Proxy')
        expect(inputValues()).toEqual(['54301'])
    })
})
