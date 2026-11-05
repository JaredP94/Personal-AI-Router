// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// @vitest-environment jsdom

import { act, createElement } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { LoadOptions } from '@/ui/components/ModelManager/LoadOptions'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useNodesStore } from '@/ui/stores/nodes.store'
import type {
    LoadProfileTarget,
    LoadProfileSnapshot,
    LoadProfileRequest,
    LoadProfilePreview
} from '@/shared/types/load-profile'
import type { NodeItem } from '@/shared/types/nodes'
import type { EngineType } from '@/shared/types/engines'

const baseline: LoadProfileSnapshot = {
    nodeId: 'local',
    engine: 'lmstudio',
    model: 'owner/model',
    revision: 1,
    loadText: '--gpu max',
    format: 'pair-arguments-v1',
    editable: true,
    reason: ''
}
const peer: NodeItem = {
    id: 'peer',
    hostUuid: 'peer',
    peerRecordId: 'verified-proof',
    identityVerification: 'verified',
    name: 'Peer',
    ipAddress: '192.0.2.10',
    port: 14318,
    allIpAddresses: [],
    status: 'active',
    os: 'Linux',
    topology: { gpus: [], cpu: { model: '', cores: 0, threads: 0 }, ram: 0, storage: [] }
}
const getProfile = vi.fn<(target: LoadProfileTarget) => Promise<LoadProfileSnapshot>>()
const previewProfile = vi.fn<(request: LoadProfileRequest) => Promise<LoadProfilePreview>>()
const applyProfile = vi.fn<(request: LoadProfileRequest) => Promise<LoadProfileSnapshot>>()
let container: HTMLDivElement
let root: Root
async function render(
    nodeId = 'local',
    model = 'owner/model',
    engineType: EngineType = 'lm-studio'
): Promise<void> {
    await act(async () =>
        root.render(createElement(LoadOptions, { nodeId, model, engineType, disabled: false }))
    )
}
async function open(): Promise<void> {
    const details = container.querySelector('details')
    if (!details) throw new Error('Missing Load options')
    await act(async () => {
        details.open = true
        details.dispatchEvent(new Event('toggle'))
    })
}
function field(): HTMLTextAreaElement {
    const input = container.querySelector<HTMLTextAreaElement>(
        '[aria-label="LM Studio load options"]'
    )
    if (!input) throw new Error('Missing load options input')
    return input
}
async function change(value: string): Promise<void> {
    const input = field()
    await act(async () => {
        Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')?.set?.call(
            input,
            value
        )
        input.dispatchEvent(new Event('input', { bubbles: true }))
    })
}
function button(label: string): HTMLButtonElement {
    const result = Array.from(container.querySelectorAll('button')).find(
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
    getProfile.mockReset().mockImplementation(async target => ({ ...baseline, ...target }))
    previewProfile.mockReset().mockImplementation(async request => ({
        revision: request.expectedRevision,
        loadText: request.loadText,
        format: 'pair-arguments-v1',
        load_args: [],
        errors: {}
    }))
    applyProfile
        .mockReset()
        .mockImplementation(async request => ({ ...baseline, ...request, revision: 2, reason: '' }))
    vi.stubGlobal('pairApi', {
        engines: {
            getLoadProfile: getProfile,
            previewLoadProfile: previewProfile,
            applyLoadProfile: applyProfile
        }
    })
    useConnectionStore.setState({ connected: true, selfId: 'local' })
    useNodesStore.setState({ nodes: new Map([['peer', peer]]) })
    container = document.createElement('div')
    document.body.append(container)
    root = createRoot(container)
})
afterEach(async () => {
    await act(async () => root.unmount())
    container.remove()
    useConnectionStore.setState({ connected: false, selfId: null })
    useNodesStore.setState({ nodes: new Map() })
    vi.unstubAllGlobals()
})

describe('per-model LM Studio Load options', () => {
    it('loads lazily and disables editing until the authoritative snapshot arrives', async () => {
        let finish = (_snapshot: LoadProfileSnapshot): void => {}
        getProfile.mockImplementationOnce(
            () =>
                new Promise(resolve => {
                    finish = resolve
                })
        )
        await render()
        expect(getProfile).not.toHaveBeenCalled()
        await open()
        expect(container.textContent).toContain('Loading load options')
        expect(field().disabled).toBe(true)
        await act(async () => finish(baseline))
        expect(field().value).toBe('--gpu max')
        expect(field().disabled).toBe(false)
        expect(button('Apply').disabled).toBe(true)
    })
    it('previews the exact node/model/revision before saving without restarting the engine', async () => {
        await render()
        await open()
        await change('--gpu off --context-length 200000')
        await click('Apply')
        expect(previewProfile).toHaveBeenCalledWith({
            nodeId: 'local',
            engine: 'lmstudio',
            model: 'owner/model',
            expectedRevision: 1,
            format: 'pair-arguments-v1',
            loadText: '--gpu off --context-length 200000'
        })
        expect(applyProfile).toHaveBeenCalledWith(
            expect.objectContaining({
                nodeId: 'local',
                model: 'owner/model',
                expectedRevision: 1,
                requestId: expect.any(String),
                loadText: '--gpu off --context-length 200000'
            })
        )
        expect(previewProfile.mock.invocationCallOrder[0]).toBeLessThan(
            applyProfile.mock.invocationCallOrder[0]
        )
        expect(container.textContent).toContain('next load')
        expect(button('Apply').disabled).toBe(true)
    })
    it('shows one preview error and does not apply rejected options', async () => {
        previewProfile.mockResolvedValueOnce({
            revision: 1,
            loadText: '',
            format: 'pair-arguments-v1',
            load_args: [],
            errors: { loadText: 'PAIR owns model identity.' }
        })
        await render()
        await open()
        await change('--identifier other')
        await click('Apply')
        expect(container.querySelectorAll('[role="alert"]')).toHaveLength(1)
        expect(container.textContent).toContain('PAIR owns model identity.')
        expect(applyProfile).not.toHaveBeenCalled()
    })
    it('shows saving progress and a single safe failure, then requires refresh', async () => {
        let fail = (_error: Error): void => {}
        applyProfile.mockImplementationOnce(
            () =>
                new Promise((_resolve, reject) => {
                    fail = reject
                })
        )
        await render()
        await open()
        await change('--gpu off')
        await click('Apply')
        expect(button('Applying…').disabled).toBe(true)
        expect(field().disabled).toBe(true)
        await act(async () => fail(new Error('Load options could not be saved.')))
        expect(container.querySelectorAll('[role="alert"]')).toHaveLength(1)
        expect(button('Apply').disabled).toBe(true)
        await click('Reload saved options')
        expect(field().value).toBe('--gpu max')
    })
    it('clearing text saves vendor defaults for the next load', async () => {
        await render()
        await open()
        await change('')
        await click('Apply')
        expect(applyProfile).toHaveBeenCalledWith(expect.objectContaining({ loadText: '' }))
    })
    it('reloads on a preview revision conflict instead of overwriting another editor', async () => {
        previewProfile.mockResolvedValueOnce({
            revision: 2,
            loadText: '--gpu off',
            format: 'pair-arguments-v1',
            load_args: [],
            errors: { loadText: 'Load options changed. Reload before applying.' }
        })
        getProfile
            .mockImplementationOnce(async () => baseline)
            .mockImplementationOnce(async () => ({
                ...baseline,
                revision: 2,
                loadText: '--parallel 3'
            }))
        await render()
        await open()
        await change('--gpu off')
        await click('Apply')
        expect(field().value).toBe('--parallel 3')
        expect(container.textContent).toContain('changed elsewhere')
        expect(applyProfile).not.toHaveBeenCalled()
    })
    it('fences a delayed preview when the user edits the text', async () => {
        let finish = (_preview: LoadProfilePreview): void => {}
        previewProfile.mockImplementationOnce(
            () =>
                new Promise(resolve => {
                    finish = resolve
                })
        )
        await render()
        await open()
        await change('--gpu off')
        await click('Apply')
        await change('--parallel 3')
        await act(async () =>
            finish({
                revision: 1,
                loadText: '--gpu off',
                format: 'pair-arguments-v1',
                load_args: [],
                errors: {}
            })
        )
        expect(applyProfile).not.toHaveBeenCalled()
        expect(field().value).toBe('--parallel 3')
    })
    it('fences delayed reads and previews across model changes', async () => {
        let finish = (_preview: LoadProfilePreview): void => {}
        previewProfile.mockImplementationOnce(
            () =>
                new Promise(resolve => {
                    finish = resolve
                })
        )
        await render()
        await open()
        await change('--gpu off')
        await click('Apply')
        await render('local', 'different/model')
        await open()
        await act(async () =>
            finish({
                revision: 1,
                loadText: '--gpu off',
                format: 'pair-arguments-v1',
                load_args: [],
                errors: {}
            })
        )
        expect(applyProfile).not.toHaveBeenCalled()
        expect(getProfile).toHaveBeenLastCalledWith({
            nodeId: 'local',
            engine: 'lmstudio',
            model: 'different/model'
        })
    })
    it('does not overwrite edits made during a requested refresh', async () => {
        let finish = (_snapshot: LoadProfileSnapshot): void => {}
        await render()
        await open()
        await change('--gpu off')
        getProfile.mockImplementationOnce(
            () =>
                new Promise(resolve => {
                    finish = resolve
                })
        )
        await click('Reload saved options')
        await change('--parallel 3')
        await act(async () => finish({ ...baseline, revision: 2, loadText: '--gpu off' }))
        expect(field().value).toBe('--parallel 3')
    })
    it('supports verified peers using the canonical record key', async () => {
        await render('peer')
        await open()
        await change('--gpu off')
        await click('Apply')
        expect(applyProfile).toHaveBeenCalledWith(
            expect.objectContaining({ nodeId: 'peer', model: 'owner/model' })
        )
    })
    it.each(['preview:claim', 'manual:entry', 'claimed-host'])(
        'excludes unverified or manual record %s even with a claimed host UUID',
        async nodeId => {
            useNodesStore.setState({
                nodes: new Map([
                    [
                        nodeId,
                        {
                            ...peer,
                            id: nodeId,
                            hostUuid: 'peer',
                            peerRecordId: nodeId,
                            identityVerification: nodeId.startsWith('manual:')
                                ? 'manual'
                                : 'unverified'
                        }
                    ]
                ])
            })
            await render(nodeId)
            expect(container.textContent).toBe('')
            expect(getProfile).not.toHaveBeenCalled()
        }
    )
    it.each<EngineType>(['ollama', 'omlx'])('does not expose profiles for %s', async engine => {
        await render('local', 'model', engine)
        expect(container.textContent).toBe('')
        expect(getProfile).not.toHaveBeenCalled()
    })
    it('keeps unsupported formats and unavailable targets read-only', async () => {
        getProfile.mockResolvedValueOnce({ ...baseline, format: 'future-format' })
        await render()
        await open()
        expect(field().disabled).toBe(true)
        expect(container.textContent).toContain('not supported')
    })
    it('fences a preview after disconnect or peer revocation', async () => {
        let finish = (_preview: LoadProfilePreview): void => {}
        previewProfile.mockImplementationOnce(
            () =>
                new Promise(resolve => {
                    finish = resolve
                })
        )
        await render('peer')
        await open()
        await change('--gpu off')
        await click('Apply')
        await act(async () =>
            useNodesStore.setState({
                nodes: new Map([['peer', { ...peer, identityVerification: 'unverified' }]])
            })
        )
        await act(async () =>
            finish({
                revision: 1,
                loadText: '--gpu off',
                format: 'pair-arguments-v1',
                load_args: [],
                errors: {}
            })
        )
        expect(applyProfile).not.toHaveBeenCalled()
        expect(container.textContent).toBe('')
    })
    it('retains distinct profiles for model names containing delimiters', async () => {
        await render('peer', 'owner:model/name')
        await open()
        expect(getProfile).toHaveBeenLastCalledWith({
            nodeId: 'peer',
            engine: 'lmstudio',
            model: 'owner:model/name'
        })
        await render('local', 'owner:model/name')
        await open()
        expect(getProfile).toHaveBeenLastCalledWith({
            nodeId: 'local',
            engine: 'lmstudio',
            model: 'owner:model/name'
        })
    })
})
