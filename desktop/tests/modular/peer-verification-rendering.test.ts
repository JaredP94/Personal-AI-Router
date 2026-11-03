// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// @vitest-environment jsdom

import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import AvailableNode from '@/ui/components/ClusterSettings/AvailableNode'
import { AddNodeModal } from '@/ui/components/AddNodeModal'
import NodeCardDetails from '@/ui/components/NodeList/NodeCardDetails'
import { useNodesStore } from '@/ui/stores/nodes.store'
import { useDiscoveredNodesStore } from '@/ui/stores/discovered-nodes.store'
import { buildOverviewNodes } from '@/ui/utils/overview-nodes'
import { nodeVerificationLabel } from '@/shared/utils/node-record-key'
import type { NodeItem } from '@/shared/types/nodes'

let container: HTMLDivElement
let root: ReturnType<typeof createRoot>
const inviteNode = vi.fn().mockResolvedValue({ state: 'paired', inviteId: 'fixture' })
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
const preview: NodeItem = {
    ...peer,
    id: 'preview:claim',
    peerRecordId: 'preview:claim',
    identityVerification: 'unverified'
}
const manual: NodeItem = {
    ...peer,
    id: 'manual:config',
    peerRecordId: 'manual:config',
    identityVerification: 'manual'
}

beforeEach(() => {
    inviteNode.mockResolvedValue({ state: 'paired', inviteId: 'fixture' })
    vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true)
    vi.stubGlobal(
        'ResizeObserver',
        class {
            observe = vi.fn()
            disconnect = vi.fn()
        }
    )
    vi.stubGlobal('requestAnimationFrame', () => 1)
    vi.stubGlobal('cancelAnimationFrame', vi.fn())
    Object.defineProperty(window, 'pairApi', {
        configurable: true,
        value: { cluster: { inviteNode, abandonIfSolo: vi.fn().mockResolvedValue(undefined) } }
    })
    container = document.createElement('div')
    document.body.append(container)
    root = createRoot(container)
})
afterEach(async () => {
    await act(async () => root.unmount())
    container.remove()
    vi.unstubAllGlobals()
})

describe('node verification presentation', () => {
    it.each([preview, manual])(
        'renders $identityVerification without peer controls or telemetry',
        async node => {
            await act(async () => root.render(createElement(NodeCardDetails, { node })))
            expect(container.textContent).toContain(
                node === manual ? 'Engine only — added by address' : 'Not in your cluster'
            )
            expect(container.querySelector('[aria-label="Engine settings"]')).toBeNull()
            expect(container.querySelector('[aria-label="Show performance"]')).toBeNull()
            expect(container.textContent).not.toContain('PAIR activity')
            const pair = Array.from(container.querySelectorAll('button')).find(
                button => button.textContent === 'Pair'
            )
            if (node === preview) {
                expect(pair).toBeDefined()
                await act(async () => pair?.click())
                expect(inviteNode).toHaveBeenCalledWith('192.0.2.10')
            } else expect(pair).toBeUndefined()
        }
    )

    it.each(['verified', 'local'])(
        'keeps existing peer controls for %s records',
        async identityVerification => {
            await act(async () =>
                root.render(
                    createElement(NodeCardDetails, { node: { ...peer, identityVerification } })
                )
            )
            expect(container.querySelector('[aria-label="Engine settings"]')).not.toBeNull()
            expect(container.textContent).not.toContain('Not in your cluster')
            expect(container.textContent).not.toContain('Engine only')
        }
    )

    it('offers Pair on an unverified list row even when its public cluster claim is set', async () => {
        await act(async () =>
            root.render(
                createElement(AvailableNode, {
                    node: { ...preview, trusted: false, clustered: true, lastSeen: 0 }
                })
            )
        )
        expect(container.textContent).toContain('Not in your cluster')
        const button = container.querySelector('button')
        expect(button?.textContent).toBe('Pair')
        expect(button?.disabled).toBe(false)
        await act(async () => button?.click())
        expect(inviteNode).toHaveBeenCalledWith('192.0.2.10')
    })

    it('offers Pair in the Add node modal for a preview whose public cluster claim is set', async () => {
        // jsdom has no <dialog> modal API; the modal calls showModal()/close().
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
        useDiscoveredNodesStore.setState({
            nodes: [
                { ...preview, trusted: false, clustered: true, lastSeen: 0 },
                { ...manual, trusted: false, clustered: false, lastSeen: 0 }
            ]
        })
        await act(async () =>
            root.render(createElement(AddNodeModal, { open: true, onOpenChange: vi.fn() }))
        )
        expect(document.body.textContent).toContain('Not in your cluster')
        expect(document.body.textContent).not.toContain('In another cluster')
        const buttons = Array.from(document.body.querySelectorAll('button'))
        const pair = buttons.filter(button => button.textContent === 'Pair')
        expect(pair).toHaveLength(1)
        expect(pair[0]?.disabled).toBe(false)
        await act(async () => pair[0]?.click())
        expect(inviteNode).toHaveBeenCalledWith('192.0.2.10')
        await act(async () => root.render(null))
        useDiscoveredNodesStore.setState({ nodes: [] })
        Reflect.deleteProperty(HTMLDialogElement.prototype, 'showModal')
        Reflect.deleteProperty(HTMLDialogElement.prototype, 'close')
    })

    it('does not infer a version diagnosis from unverified metadata', () => {
        expect(nodeVerificationLabel({ ...preview, identityVerification: undefined })).toBe(
            'Not in your cluster'
        )
        expect(nodeVerificationLabel(peer)).toBe('')
        expect(nodeVerificationLabel({ ...peer, identityVerification: 'local' })).toBe('')
    })

    it('normalizes both renderer stores on snapshots and pushes, dropping invalid records', async () => {
        let upsert: ((node: NodeItem) => void) | undefined
        const invalid = { ...peer, hostUuid: 'preview:reserved' }
        const available = [peer, preview, invalid].map(node => ({
            ...node,
            trusted: true,
            clustered: true,
            lastSeen: 0
        }))
        Object.defineProperty(window, 'pairApi', {
            configurable: true,
            value: {
                nodes: {
                    getInitial: async () => ({
                        nodes: { peer, claimed: preview, invalid },
                        fetchedNodes: true
                    }),
                    onUpsert: (callback: (node: NodeItem) => void) => {
                        upsert = callback
                        return () => {}
                    },
                    onRemove: () => () => {}
                },
                discovery: { getNodes: async () => available, onNodesChanged: () => () => {} }
            }
        })
        await useNodesStore.getState().initialize()
        await useDiscoveredNodesStore.getState().initialize()
        expect([...useNodesStore.getState().nodes.keys()].sort()).toEqual(['peer', 'preview:claim'])
        expect(useDiscoveredNodesStore.getState().nodes.map(node => node.id)).toEqual([
            'peer',
            'preview:claim'
        ])
        upsert?.({ ...preview, id: 'peer' })
        upsert?.(invalid)
        expect([...useNodesStore.getState().nodes.keys()].sort()).toEqual(['peer', 'preview:claim'])
        expect(useNodesStore.getState().nodes.get('peer')?.identityVerification).toBe('verified')
        useNodesStore.getState().cleanup()
        useDiscoveredNodesStore.getState().cleanup()
    })

    it('the overview normalizes record keys instead of accepting claimed map keys', () => {
        const nodes = new Map([
            ['peer', preview],
            ['claimed-map-key', peer]
        ])
        const result = buildOverviewNodes(nodes, [], 'peer', 'Linux')
        expect(result.map(node => node.id)).toEqual(['peer', 'preview:claim'])
        expect(result[0].identityVerification).toBe('verified')
        expect(buildOverviewNodes(new Map(), [], 'manual:reserved', 'Linux')).toEqual([])
    })

    it('keeps claimed peers separate in the overview without granting membership', () => {
        const nodes = new Map([
            [peer.id, peer],
            [preview.id, preview],
            [manual.id, manual]
        ])
        expect(buildOverviewNodes(nodes, [], 'peer', 'Linux').map(node => node.id)).toEqual([
            'peer',
            'preview:claim',
            'manual:config'
        ])
    })
})
