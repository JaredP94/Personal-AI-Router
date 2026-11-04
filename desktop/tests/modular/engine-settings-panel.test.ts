// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// @vitest-environment jsdom

import { act, createElement } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { EngineSettingsSection } from '@/ui/components/BackendRow/EngineSettingsSection'
import { useEngineSettingsStore } from '@/ui/stores/engine-settings.store'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useNodesStore } from '@/ui/stores/nodes.store'
import type {
    EngineSettingsPreview,
    EngineSettingsRequest,
    EngineSettingsReceipt,
    EngineSettingsSnapshot,
    EngineSettingsTarget
} from '@/shared/types/engine-settings'
import type { EngineType } from '@/shared/types/engines'
import type { NodeItem } from '@/shared/types/nodes'

const baseline: EngineSettingsSnapshot = {
    nodeId: 'local',
    engine: 'ollama',
    revision: 1,
    appliedRevision: 1,
    sequence: 1,
    epoch: 'one',
    settings: { serverPort: 11435, proxyPort: 11434, launchText: '' },
    effectiveServerPort: 11435,
    effectiveProxyPort: 11434,
    running: true,
    adopted: false,
    editable: true,
    reason: '',
    format: 'pair-arguments-v1',
    phase: 'idle',
    error: '',
    requestId: ''
}
const peer: NodeItem = {
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
    topology: { gpus: [], cpu: { model: '', cores: 0, threads: 0 }, ram: 0, storage: [] }
}
const getSettings = vi.fn<(target: EngineSettingsTarget) => Promise<EngineSettingsSnapshot>>()
const previewSettings = vi.fn<(request: EngineSettingsRequest) => Promise<EngineSettingsPreview>>()
const applySettings = vi.fn<(request: EngineSettingsRequest) => Promise<EngineSettingsReceipt>>()
let deliver = (_snapshot: EngineSettingsSnapshot): void => {}
let disconnect = (_target: { nodeId: string }): void => {}
let container: HTMLDivElement
let root: Root

async function render(engineType: EngineType = 'ollama', nodeId = 'local'): Promise<void> {
    await act(async () =>
        root.render(createElement(EngineSettingsSection, { nodeId, engineType, disabled: false }))
    )
}
function field(label: string): HTMLInputElement | HTMLTextAreaElement {
    const input = container.querySelector<HTMLInputElement | HTMLTextAreaElement>(
        `[aria-label="${label}"]`
    )
    if (!input) throw new Error(`Missing ${label}`)
    return input
}
async function change(label: string, value: string): Promise<void> {
    const input = field(label)
    const prototype =
        input instanceof HTMLTextAreaElement
            ? HTMLTextAreaElement.prototype
            : HTMLInputElement.prototype
    await act(async () => {
        Object.getOwnPropertyDescriptor(prototype, 'value')?.set?.call(input, value)
        input.dispatchEvent(new Event('input', { bubbles: true }))
    })
}
function button(label: string): HTMLButtonElement {
    const result = Array.from(document.querySelectorAll('button')).find(
        b => b.textContent?.trim() === label
    )
    if (!result) throw new Error(`Missing ${label}`)
    return result
}
async function click(label: string): Promise<void> {
    await act(async () => button(label).click())
}

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
        }
    })
    getSettings.mockReset().mockImplementation(async target => ({ ...baseline, ...target }))
    previewSettings.mockReset().mockImplementation(async request => ({
        settings: request.settings,
        revision: 1,
        errors: {},
        restart: false,
        rebind: true
    }))
    applySettings.mockReset().mockImplementation(async request => {
        deliver({
            ...baseline,
            ...request,
            requestId: request.requestId ?? '',
            revision: 2,
            sequence: 2,
            appliedRevision: 2,
            phase: 'succeeded'
        })
        return { revision: 2, phase: 'succeeded' }
    })
    vi.stubGlobal('pairApi', {
        engines: {
            getSettings,
            previewSettings,
            applySettings,
            onSettingsChanged: (callback: typeof deliver) => {
                deliver = callback
                return () => {}
            },
            onSettingsDisconnected: (callback: typeof disconnect) => {
                disconnect = callback
                return () => {}
            }
        }
    })
    useEngineSettingsStore.setState({ entries: {} })
    useConnectionStore.setState({ connected: true, selfId: 'local' })
    useNodesStore.setState({ nodes: new Map([['peer', peer]]) })
    container = document.createElement('div')
    document.body.append(container)
    root = createRoot(container)
})
afterEach(async () => {
    await act(async () => root.unmount())
    container.remove()
    useEngineSettingsStore.setState({ entries: {} })
    useNodesStore.setState({ nodes: new Map() })
    useConnectionStore.setState({ connected: false, selfId: null })
    Reflect.deleteProperty(HTMLDialogElement.prototype, 'showModal')
    Reflect.deleteProperty(HTMLDialogElement.prototype, 'close')
    vi.unstubAllGlobals()
})

describe('engine launch settings panel', () => {
    it('loads read-only, then enables editing when settings arrive', async () => {
        let finish = (_snapshot: EngineSettingsSnapshot): void => {}
        getSettings.mockImplementationOnce(
            () =>
                new Promise(resolve => {
                    finish = resolve
                })
        )
        await render()
        expect(container.textContent).toContain('Loading settings')
        expect(field('Proxy port').disabled).toBe(true)
        await act(async () => finish(baseline))
        expect(field('Server port').value).toBe('11435')
        expect(field('Proxy port').disabled).toBe(false)
    })

    it.each<EngineType>(['ollama', 'lm-studio'])(
        'previews full %s settings before applying without an unnecessary confirmation',
        async engineType => {
            await render(engineType)
            await change('Proxy port', '12000')
            await change('Engine arguments', 'PRIVATE=value --option 3')
            expect(button('Apply').disabled).toBe(false)
            await click('Apply')
            expect(previewSettings).toHaveBeenCalledWith({
                nodeId: 'local',
                engine: engineType === 'lm-studio' ? 'lmstudio' : 'ollama',
                expectedRevision: 1,
                settings: {
                    serverPort: 11435,
                    proxyPort: 12000,
                    launchText: 'PRIVATE=value --option 3'
                }
            })
            expect(applySettings).toHaveBeenCalledWith(
                expect.objectContaining({
                    expectedRevision: 1,
                    requestId: expect.any(String),
                    settings: {
                        serverPort: 11435,
                        proxyPort: 12000,
                        launchText: 'PRIVATE=value --option 3'
                    }
                })
            )
            const outsideEditor = container.cloneNode(true)
            if (!(outsideEditor instanceof HTMLElement)) throw new Error('Expected an element')
            outsideEditor.querySelector('textarea')?.remove()
            expect(outsideEditor.textContent).not.toContain('PRIVATE=value')
            expect(document.body.textContent).not.toContain('Restart engine?')
        }
    )

    it('shows field errors without applying', async () => {
        previewSettings.mockResolvedValueOnce({
            settings: baseline.settings,
            revision: 1,
            errors: { proxyPort: 'Port is already in use.' },
            restart: false,
            rebind: false
        })
        await render()
        await change('Proxy port', '12000')
        await click('Apply')
        expect(container.textContent).toContain('Port is already in use.')
        expect(applySettings).not.toHaveBeenCalled()
    })

    it('rejects invalid numeric input rather than truncating or stripping it', async () => {
        await render()
        await change('Server port', '12oops')
        await click('Apply')
        expect(container.textContent).toContain('1 to 65535')
        expect(previewSettings).not.toHaveBeenCalled()
        expect(applySettings).not.toHaveBeenCalled()
    })

    it('requires restart confirmation only when preview requests it', async () => {
        previewSettings.mockImplementation(async request => ({
            settings: request.settings,
            revision: 1,
            errors: {},
            restart: true,
            rebind: false
        }))
        await render()
        await change('Server port', '12000')
        await click('Apply')
        expect(document.body.textContent).toContain('Restart engine?')
        expect(applySettings).not.toHaveBeenCalled()
        await click('Cancel')
        expect(applySettings).not.toHaveBeenCalled()
        await click('Apply')
        await click('Restart')
        expect(applySettings).toHaveBeenCalledTimes(1)
    })

    it('shows apply progress and one terminal failure message', async () => {
        let finish = (): void => {}
        applySettings.mockImplementationOnce(
            request =>
                new Promise(resolve => {
                    finish = () => {
                        deliver({
                            ...baseline,
                            ...request,
                            requestId: request.requestId ?? '',
                            sequence: 3,
                            revision: 2,
                            phase: 'failed',
                            error: 'Requested port is busy.'
                        })
                        resolve({ revision: 2, phase: 'failed' })
                    }
                })
        )
        await render()
        await change('Proxy port', '12000')
        await click('Apply')
        expect(button('Applying…').disabled).toBe(true)
        expect(field('Proxy port').disabled).toBe(true)
        await act(async () => finish())
        expect(container.querySelectorAll('[role="alert"]')).toHaveLength(1)
        expect(container.textContent).toContain('Requested port is busy.')
        expect(button('Apply').disabled).toBe(false)
    })

    it('renders the owning node’s restored-settings failure outcome without losing desired settings', async () => {
        applySettings.mockImplementationOnce(async request => {
            deliver({
                ...baseline,
                ...request,
                requestId: request.requestId ?? '',
                sequence: 2,
                revision: 2,
                phase: 'failed',
                effectiveServerPort: 11435,
                effectiveProxyPort: 11434,
                error: 'Engine restart failed; previous settings were restored.'
            })
            return { revision: 2, phase: 'failed' }
        })
        await render()
        await change('Server port', '12000')
        await click('Apply')
        const alerts = container.querySelectorAll('[role="alert"]')
        expect(alerts).toHaveLength(1)
        expect(alerts[0].textContent).toBe(
            'Engine restart failed; previous settings were restored.'
        )
        expect(field('Server port').value).toBe('12000')
        expect(button('Apply').disabled).toBe(false)
    })

    it('shows a single safe apply rejection message', async () => {
        applySettings.mockRejectedValueOnce(new Error('Port is unavailable.'))
        await render()
        await change('Proxy port', '12000')
        await click('Apply')
        expect(container.querySelectorAll('[role="alert"]')).toHaveLength(1)
        expect(container.textContent).toContain('Port is unavailable.')
    })

    it('reloads and explains revision conflicts returned during apply', async () => {
        applySettings.mockImplementationOnce(async () => {
            getSettings.mockResolvedValue({
                ...baseline,
                revision: 2,
                sequence: 2,
                settings: { ...baseline.settings, proxyPort: 13000 }
            })
            throw new Error('settings changed; reload before applying')
        })
        await render()
        await change('Proxy port', '12000')
        await click('Apply')
        expect(container.textContent).toContain('Settings changed elsewhere')
        expect(field('Proxy port').value).toBe('13000')
        expect(button('Apply').disabled).toBe(true)
    })

    it('does not submit a stale draft after an external revision changes', async () => {
        await render()
        await change('Proxy port', '12000')
        await act(async () =>
            deliver({
                ...baseline,
                sequence: 2,
                revision: 2,
                settings: { ...baseline.settings, proxyPort: 13000 }
            })
        )
        await click('Apply')
        expect(container.textContent).toContain('Settings changed elsewhere')
        expect(field('Proxy port').value).toBe('13000')
        expect(applySettings).not.toHaveBeenCalled()
    })

    it('discards a delayed preview when the editor changes', async () => {
        let finish = (_result: EngineSettingsPreview): void => {}
        previewSettings.mockImplementationOnce(
            () =>
                new Promise(resolve => {
                    finish = resolve
                })
        )
        await render()
        await change('Proxy port', '12000')
        await click('Apply')
        await change('Proxy port', '13000')
        await act(async () =>
            finish({
                settings: { ...baseline.settings, proxyPort: 12000 },
                revision: 1,
                errors: {},
                restart: false,
                rebind: true
            })
        )
        expect(field('Proxy port').value).toBe('13000')
        expect(applySettings).not.toHaveBeenCalled()
    })

    it('does not apply a delayed preview after the settings stream disconnects', async () => {
        let finish = (_result: EngineSettingsPreview): void => {}
        previewSettings.mockImplementationOnce(
            () =>
                new Promise(resolve => {
                    finish = resolve
                })
        )
        await render()
        await change('Proxy port', '12000')
        await click('Apply')
        await act(async () => disconnect({ nodeId: 'local' }))
        await act(async () =>
            finish({
                settings: { ...baseline.settings, proxyPort: 12000 },
                revision: 1,
                errors: {},
                restart: false,
                rebind: true
            })
        )
        expect(field('Proxy port').disabled).toBe(true)
        expect(applySettings).not.toHaveBeenCalled()
    })

    it('reloads after preview rejects a stale expectedRevision', async () => {
        previewSettings.mockImplementationOnce(async () => {
            getSettings.mockResolvedValue({
                ...baseline,
                revision: 2,
                sequence: 2,
                settings: { ...baseline.settings, proxyPort: 13000 }
            })
            throw new Error('settings changed on this device; reload before applying')
        })
        await render()
        await change('Proxy port', '12000')
        await click('Apply')
        expect(field('Proxy port').value).toBe('13000')
        expect(container.textContent).toContain('Settings changed elsewhere')
        expect(applySettings).not.toHaveBeenCalled()
    })

    it('oMLX exposes only the proxy port for editing', async () => {
        await render('omlx')
        expect(field('Server port').readOnly).toBe(true)
        expect(container.textContent).toContain(
            "oMLX's server settings are managed in the oMLX app"
        )
        expect(container.querySelector('textarea')).toBeNull()
        expect(field('Proxy port').disabled).toBe(false)
        await change('Proxy port', '12000')
        await click('Apply')
        expect(applySettings).toHaveBeenCalledWith(
            expect.objectContaining({
                engine: 'omlx',
                settings: { serverPort: 11435, proxyPort: 12000, launchText: '' }
            })
        )
    })

    it.each(['preview:claim', 'manual:config'])(
        'excludes %s even with a claimed host identity',
        async nodeId => {
            useNodesStore.setState({
                nodes: new Map([
                    [
                        nodeId,
                        {
                            ...peer,
                            id: nodeId,
                            peerRecordId: nodeId,
                            identityVerification: nodeId.startsWith('manual:')
                                ? 'manual'
                                : 'unverified'
                        }
                    ]
                ])
            })
            await render('ollama', nodeId)
            expect(container.textContent).toBe('')
            expect(getSettings).not.toHaveBeenCalled()
        }
    )

    it('includes verified paired devices', async () => {
        await render('ollama', 'peer')
        expect(field('Proxy port').disabled).toBe(false)
        await change('Proxy port', '12000')
        await click('Apply')
        expect(applySettings).toHaveBeenCalledWith(expect.objectContaining({ nodeId: 'peer' }))
    })

    it('keeps unsupported formats and disconnected devices read-only', async () => {
        getSettings.mockResolvedValue({ ...baseline, format: 'future-format' })
        await render()
        expect(field('Proxy port').disabled).toBe(true)
        expect(container.textContent).toContain('not supported')
        await act(async () => useConnectionStore.setState({ connected: false }))
        expect(button('Apply').disabled).toBe(true)
    })
})
