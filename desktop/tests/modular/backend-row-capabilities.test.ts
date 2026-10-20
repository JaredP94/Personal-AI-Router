// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// @vitest-environment jsdom

import { act, createElement } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { EngineCapabilityFlags } from '@/shared/types/engines'
import type { BackendInfo } from '@/ui/types/engine-info'
import { BackendRow } from '@/ui/components/BackendRow/BackendRow'
import { useEngineStatusStore } from '@/ui/stores/engine-status.store'
import { useConnectionStore } from '@/ui/stores/connection.store'

const supported: EngineCapabilityFlags = {
    listModels: true,
    pullModel: true,
    deleteModel: true,
    deleteRestartsEngine: false,
    setPort: true,
    install: true,
    uninstall: true
}
const backend: BackendInfo = {
    type: 'ollama',
    displayName: 'Ollama',
    source: 'installed',
    processStatus: 'stopped',
    port: 11434,
    proxyPort: 54301,
    models: [],
    updateAvailable: {
        currentVersion: '1',
        latestVersion: '2',
        releaseUrl: 'https://example.test',
        installType: 'managed'
    }
}
let container: HTMLDivElement
let root: Root
const install = vi.fn()
const update = vi.fn()

function render(flags: EngineCapabilityFlags | null, installed = true): void {
    act(() => {
        useEngineStatusStore.setState({
            statusByNode: flags
                ? new Map([
                      [
                          'local',
                          new Map([
                              [
                                  'ollama',
                                  {
                                      engineType: 'ollama',
                                      nodeId: 'local',
                                      processStatus: installed ? 'stopped' : 'not-installed',
                                      capabilities: flags,
                                      enginePort: 11434,
                                      proxyPort: 54301
                                  }
                              ]
                          ])
                      ]
                  ])
                : new Map()
        })
        root.render(
            createElement(BackendRow, {
                backend: { ...backend, processStatus: installed ? 'stopped' : 'not-installed' },
                nodeId: 'local',
                statusKnown: true
            })
        )
    })
}

function buttons(): HTMLButtonElement[] {
    return Array.from(container.querySelectorAll('button'))
}

function portValues(): string[] {
    return Array.from(
        container.querySelectorAll<HTMLInputElement>('input:not([type="checkbox"])')
    ).map(input => input.value)
}

function expand(): void {
    const title = Array.from(container.querySelectorAll<HTMLElement>('*')).find(
        element => element.childElementCount === 0 && element.textContent === 'Ollama'
    )
    expect(title).toBeDefined()
    act(() => title?.click())
}

describe('BackendRow reported capability gates', () => {
    beforeEach(() => {
        vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true)
        vi.stubGlobal('pairApi', { engines: { install, update } })
        vi.stubGlobal('windowApi', { platform: 'macOS' })
        useConnectionStore.setState({ selfId: 'local' })
        container = document.createElement('div')
        document.body.appendChild(container)
        root = createRoot(container)
    })
    afterEach(() => {
        act(() => root.unmount())
        container.remove()
        useEngineStatusStore.setState({ statusByNode: new Map() })
        useConnectionStore.setState({ selfId: null })
        vi.unstubAllGlobals()
    })

    it('offers install only when the owning node reports install', () => {
        render({ ...supported, install: false }, false)
        expect(buttons().find(button => button.textContent === 'Install')).toBeUndefined()
        render(supported, false)
        const button = buttons().find(button => button.textContent === 'Install')
        expect(button).toBeDefined()
        act(() => button?.click())
        expect(install).toHaveBeenCalledExactlyOnceWith('ollama', 'local')
    })

    it('does not offer uninstall or reinstall-update without the required capabilities', () => {
        render({ ...supported, uninstall: false })
        expand()
        expect(buttons().find(button => button.textContent === 'Uninstall')).toBeUndefined()
        const updateButton = buttons().find(button => button.textContent === 'Update')
        expect(updateButton === undefined || updateButton.disabled).toBe(true)
        act(() => updateButton?.click())
        expect(update).not.toHaveBeenCalled()
        render(supported)
        expect(buttons().find(button => button.textContent === 'Uninstall')).toBeDefined()
        expect(buttons().find(button => button.textContent === 'Update')?.disabled).toBe(false)
        render({ ...supported, install: false })
        const reinstallButton = buttons().find(button => button.textContent === 'Update')
        expect(reinstallButton === undefined || reinstallButton.disabled).toBe(true)
    })

    it('hides the server-port field but keeps the proxy field when set_port is false', () => {
        render({ ...supported, setPort: false })
        expand()
        expect(portValues()).toEqual(['54301'])
        render(supported)
        expect(portValues()).toContain('11434')
    })

    it('renders missing reports without an unstable capability selector', () => {
        render(null)
        expand()
        expect(buttons().find(button => button.textContent === 'Uninstall')).toBeUndefined()
        expect(portValues()).toEqual(['54301'])
    })
})
