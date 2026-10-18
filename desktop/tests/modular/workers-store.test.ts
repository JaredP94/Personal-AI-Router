// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// @vitest-environment jsdom

import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { useWorkersStore } from '@/ui/stores/workers.store'
import { createPairApi } from '@/ui/api/pair-api'
import { pairTransport } from '@/preload/api/pair-transport.api'
import type { WorkerStatus } from '@/shared/types/workers'

const worker: WorkerStatus = {
    name: 'scanner',
    state: 'running',
    attempts: 0,
    pid: 42,
    restartOnHang: true,
    since: '2026-10-18T12:00:00Z'
}

/** A fake `workers` sub-API that captures the `workers:update` subscriber. */
function installFakeWorkersApi(): {
    getStatus: ReturnType<typeof vi.fn>
    restart: ReturnType<typeof vi.fn>
    push: (workers: WorkerStatus[]) => void
    subscribed: () => boolean
} {
    let subscriber: ((workers: WorkerStatus[]) => void) | null = null
    const getStatus = vi.fn(async () => [worker])
    const restart = vi.fn(async () => worker)
    window.pairApi = {
        ...createPairApi(pairTransport),
        workers: {
            getStatus,
            restart,
            onUpdate: callback => {
                subscriber = callback
                return () => {
                    subscriber = null
                }
            }
        }
    }
    return {
        getStatus,
        restart,
        push: workers => subscriber?.(workers),
        subscribed: () => subscriber !== null
    }
}

let api: ReturnType<typeof installFakeWorkersApi>

beforeEach(() => {
    api = installFakeWorkersApi()
    useWorkersStore.setState({ workers: [] })
})

afterEach(() => {
    useWorkersStore.getState().cleanup()
})

it('seeds from workers:get-status and replaces the whole list on every push', async () => {
    await useWorkersStore.getState().initialize()
    expect(api.getStatus).toHaveBeenCalledTimes(1)
    expect(useWorkersStore.getState().workers).toEqual([worker])

    api.push([{ ...worker, state: 'degraded', lastError: 'probe failed' }])
    expect(useWorkersStore.getState().workers).toEqual([
        { ...worker, state: 'degraded', lastError: 'probe failed' }
    ])

    // `workers:update` is a full snapshot, so an empty one is authoritative.
    api.push([])
    expect(useWorkersStore.getState().workers).toEqual([])
})

it('re-reads the whole snapshot on refresh', async () => {
    await useWorkersStore.getState().initialize()
    const missing = { ...worker, state: 'missing' as const }
    api.getStatus.mockResolvedValueOnce([missing])
    await useWorkersStore.getState().refresh()
    expect(api.getStatus).toHaveBeenCalledTimes(2)
    expect(useWorkersStore.getState().workers).toEqual([missing])
})

it('relays one restart by name and leaves the broker rejection to the caller', async () => {
    await expect(useWorkersStore.getState().restartWorker('scanner')).resolves.toEqual(worker)
    expect(api.restart).toHaveBeenCalledExactlyOnceWith('scanner')

    api.restart.mockRejectedValueOnce(new Error('unknown worker name'))
    await expect(useWorkersStore.getState().restartWorker('nope')).rejects.toThrow(
        'unknown worker name'
    )
})

it('drops the rows and the subscription on cleanup', async () => {
    await useWorkersStore.getState().initialize()
    api.push([worker])
    expect(useWorkersStore.getState().workers).toEqual([worker])

    useWorkersStore.getState().cleanup()
    expect(api.subscribed()).toBe(false)
    expect(useWorkersStore.getState().workers).toEqual([])

    // A late push from the closed connection must not repopulate the card.
    api.push([worker])
    expect(useWorkersStore.getState().workers).toEqual([])
})
