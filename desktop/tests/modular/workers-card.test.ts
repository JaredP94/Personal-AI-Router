// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// @vitest-environment jsdom

import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import WorkersCard from '@/ui/components/ServiceSettings/WorkersCard'
import { useWorkersStore } from '@/ui/stores/workers.store'
import { createPairApi } from '@/ui/api/pair-api'
import { pairTransport } from '@/preload/api/pair-transport.api'
import type { WorkerStatus } from '@/shared/types/workers'

const { invoke } = vi.hoisted(() => ({ invoke: vi.fn() }))
vi.mock('electron', () => ({ ipcRenderer: { invoke, on: vi.fn(), removeListener: vi.fn() } }))

const worker: WorkerStatus = {
    name: 'engine-manager',
    state: 'degraded',
    attempts: 2,
    lastError: 'probe failed',
    restartOnHang: false,
    since: '2026-10-18T12:00:00Z'
}
const container = document.createElement('div')
let root: ReturnType<typeof createRoot>

beforeEach(async () => {
    vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true)
    // jsdom ships no <dialog> modal implementation; the shared ConfirmModal
    // calls showModal()/close() on mount. The dialog still renders its content.
    HTMLDialogElement.prototype.showModal = function showModal() {
        this.open = true
    }
    HTMLDialogElement.prototype.close = function close() {
        this.open = false
    }
    document.body.append(container)
    root = createRoot(container)
    window.pairApi = createPairApi(pairTransport)
    invoke.mockResolvedValue({ success: true, data: worker })
    useWorkersStore.setState({ workers: [worker] })
    await act(async () => root.render(createElement(WorkersCard)))
})

afterEach(async () => {
    await act(async () => root.unmount())
    container.remove()
    vi.unstubAllGlobals()
})

async function click(label: string) {
    const button = Array.from(document.querySelectorAll('button')).find(
        button => button.textContent === label
    )
    if (!button) throw new Error(`Missing button: ${label}`)
    await act(async () => button.click())
}

it('renders every broker state, attempts, errors and the manual-restart hint', async () => {
    const states: WorkerStatus['state'][] = [
        'running',
        'degraded',
        'restarting',
        'exhausted',
        'missing'
    ]
    await act(async () =>
        useWorkersStore.setState({
            workers: states.map(state => ({ ...worker, name: state, state }))
        })
    )
    for (const state of states) expect(container.textContent).toContain(state)
    expect(container.textContent).toContain('Attempts: 2')
    expect(container.textContent).toContain('probe failed')
    expect(container.textContent).toContain('Not responding — restart manually')
    expect(container.querySelectorAll('button')).toHaveLength(5)
})

it('sends no engine-manager restart on cancel, then exactly one on confirm through preload', async () => {
    await click('Restart')
    expect(document.body.textContent).toContain(
        'Engines PAIR started may keep running or stop; running inference may be interrupted.'
    )
    expect(invoke).not.toHaveBeenCalled()
    await click('Cancel')
    expect(invoke).not.toHaveBeenCalled()
    await click('Restart')
    await click('Restart worker')
    expect(invoke).toHaveBeenCalledExactlyOnceWith('service-bridge:invoke', {
        channel: 'workers:restart',
        payload: { name: 'engine-manager' }
    })
})

it.each(['proxy', 'lmstudio-proxy', 'omlx-proxy'])(
    'confirms interruption before restarting %s',
    async name => {
        await act(async () => useWorkersStore.setState({ workers: [{ ...worker, name }] }))
        await click('Restart')
        expect(document.body.textContent).toContain(
            'Running inference through this proxy will be interrupted.'
        )
        await click('Cancel')
        expect(invoke).not.toHaveBeenCalled()
    }
)

it('restarts a child-free worker without confirmation and displays a failed request', async () => {
    await act(async () => useWorkersStore.setState({ workers: [{ ...worker, name: 'scanner' }] }))
    invoke.mockResolvedValue({ success: false, error: 'worker unavailable' })
    await click('Restart')
    expect(container.textContent).toContain('worker unavailable')
    expect(invoke).toHaveBeenCalledExactlyOnceWith('service-bridge:invoke', {
        channel: 'workers:restart',
        payload: { name: 'scanner' }
    })
})
