// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// @vitest-environment jsdom

import { act, createElement } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { AddNodeModal } from '@/ui/components/AddNodeModal'
import ManualNodeItem from '@/ui/components/ClusterSettings/ManualNodeItem'
import ManualEntriesSection from '@/ui/components/ClusterSettings/ManualEntriesSection'
import type { ManualNodeStatus } from '@/shared/types/manual-nodes'

let container: HTMLDivElement
let root: Root
const api = {
    manualNodes: { list: vi.fn(), add: vi.fn(), remove: vi.fn() },
    cluster: {
        inviteNode: vi.fn(),
        inviteStatus: vi.fn(),
        cancelInvite: vi.fn(),
        abandonIfSolo: vi.fn()
    },
    discovery: {
        getNodes: vi.fn().mockResolvedValue([]),
        onNodesChanged: vi.fn().mockReturnValue(() => {})
    }
}

function button(label: string): HTMLButtonElement {
    const found = Array.from(container.querySelectorAll('button')).find(
        b => b.textContent === label
    )
    if (!found) throw new Error(`Missing button ${label}`)
    return found
}

async function address(value: string) {
    const input = container.querySelector('input')
    if (!input) throw new Error('Missing address input')
    await act(async () => {
        Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')?.set?.call(
            input,
            value
        )
        input.dispatchEvent(new Event('input', { bubbles: true }))
    })
}

const pairNode: ManualNodeStatus = {
    id: 'manual:overlay.example',
    address: 'overlay.example',
    kind: 'pair',
    identityVerification: 'unverified'
}
const engineNode: ManualNodeStatus = {
    id: 'manual:engine.example',
    address: 'engine.example',
    kind: 'engine',
    identityVerification: 'manual'
}

describe('manual entries UI', () => {
    beforeEach(() => {
        vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true)
        HTMLDialogElement.prototype.showModal = function showModal() {
            this.open = true
        }
        HTMLDialogElement.prototype.close = function close() {
            this.open = false
        }
        container = document.createElement('div')
        document.body.appendChild(container)
        root = createRoot(container)
        vi.stubGlobal('pairApi', api)
        api.manualNodes.list.mockResolvedValue([])
        api.manualNodes.add.mockResolvedValue(pairNode)
        api.manualNodes.remove.mockResolvedValue({ removed: true })
        api.cluster.inviteNode.mockResolvedValue({
            inviteId: 'invite',
            state: 'pending',
            pin: '123456'
        })
    })
    afterEach(() => {
        act(() => root.unmount())
        container.remove()
        vi.resetAllMocks()
        vi.unstubAllGlobals()
    })

    it('persists PAIR intent before pairing and keeps the hostname as entered', async () => {
        let saved: ((status: ManualNodeStatus) => void) | undefined
        api.manualNodes.add.mockImplementationOnce(
            () =>
                new Promise<ManualNodeStatus>(resolve => {
                    saved = resolve
                })
        )
        await act(async () => {
            root.render(createElement(AddNodeModal, { open: true, onOpenChange: vi.fn() }))
        })
        await address('  Workstation.tail.example  ')
        await act(async () => button('Invite').click())
        expect(api.manualNodes.add).toHaveBeenCalledWith({
            address: 'Workstation.tail.example',
            kind: 'pair'
        })
        expect(api.cluster.inviteNode).not.toHaveBeenCalled()
        await act(async () => saved?.(pairNode))
        expect(api.cluster.inviteNode).toHaveBeenCalledWith('Workstation.tail.example')
    })

    it('registers an explicitly chosen engine without PIN pairing', async () => {
        const onOpenChange = vi.fn()
        await act(async () => {
            root.render(createElement(AddNodeModal, { open: true, onOpenChange }))
        })
        await act(async () => button('Engine only').click())
        expect(container.textContent).toContain('trusts no PAIR claims')
        await address('engine.example')
        await act(async () => button('Add engine').click())
        expect(api.manualNodes.add).toHaveBeenCalledWith({
            address: 'engine.example',
            kind: 'engine'
        })
        expect(api.cluster.inviteNode).not.toHaveBeenCalled()
        expect(onOpenChange).toHaveBeenCalledWith(false)
    })

    it('does not start pairing when persistence fails', async () => {
        api.manualNodes.add.mockRejectedValueOnce(new Error('Address was not saved'))
        await act(async () => {
            root.render(createElement(AddNodeModal, { open: true, onOpenChange: vi.fn() }))
        })
        await address('overlay.example')
        await act(async () => button('Invite').click())
        expect(container.textContent).toContain('Address was not saved')
        expect(api.cluster.inviteNode).not.toHaveBeenCalled()
    })

    it('keeps the saved PAIR entry when pairing fails', async () => {
        api.cluster.inviteNode.mockRejectedValueOnce(new Error('Peer offline'))
        await act(async () => {
            root.render(createElement(AddNodeModal, { open: true, onOpenChange: vi.fn() }))
        })
        await address('overlay.example')
        await act(async () => button('Invite').click())
        expect(container.textContent).toContain('Peer offline')
        expect(api.manualNodes.add).toHaveBeenCalledOnce()
        expect(api.manualNodes.remove).not.toHaveBeenCalled()
    })

    it('lists entries, labels their kind and retries unverified PAIR pairing', async () => {
        api.manualNodes.list.mockResolvedValue([pairNode, engineNode])
        await act(async () => root.render(createElement(ManualEntriesSection)))
        expect(container.textContent).toContain('overlay.example')
        expect(container.textContent).toContain('engine.example')
        expect(container.textContent).toContain('Unverified')
        expect(container.textContent).toContain('Manual bypass')
        await act(async () => button('Retry pairing').click())
        expect(api.cluster.inviteNode).toHaveBeenCalledWith('overlay.example')
    })

    it('removes a manual entry and refreshes the list', async () => {
        const onChanged = vi.fn()
        await act(async () =>
            root.render(
                createElement(ManualNodeItem, {
                    node: engineNode,
                    onChanged,
                    onError: vi.fn()
                })
            )
        )
        await act(async () => button('Remove').click())
        expect(api.manualNodes.remove).toHaveBeenCalledWith(engineNode.id)
        expect(onChanged).toHaveBeenCalledOnce()
        expect(api.cluster.inviteNode).not.toHaveBeenCalled()
    })
})
