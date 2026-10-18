// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { create } from 'zustand'
import type { WorkerStatus } from '@/shared/types/workers'

interface WorkersStore {
    workers: WorkerStatus[]
    initialize: () => Promise<void>
    refresh: () => Promise<void>
    /** Ask the broker to restart one worker; it decides what that means. */
    restartWorker: (name: string) => Promise<WorkerStatus>
    cleanup: () => void
}

let unsubs: Array<() => void> = []
let generation = 0
let revision = 0

/**
 * The broker-reported lifecycle of every supervised worker, in startup order.
 * This store holds no health or retry logic: a `workers:update` push replaces
 * the list wholesale, and `workers:get-status` re-reads it after a reconnect.
 */
export const useWorkersStore = create<WorkersStore>((set, get) => ({
    workers: [],

    restartWorker: name => {
        if (!window.pairApi) return Promise.reject(new Error('The service is not connected'))
        return window.pairApi.workers.restart(name)
    },

    refresh: async () => {
        const lifetime = generation
        const baseline = revision
        try {
            const workers = await window.pairApi.workers.getStatus()
            if (lifetime !== generation || baseline !== revision) return
            revision++
            set({ workers })
        } catch (error) {
            console.error('Failed to refresh workers store:', error)
        }
    },

    initialize: async () => {
        get().cleanup()
        const lifetime = generation
        // Subscribe before fetching: a push received during the baseline read
        // must win, including on the first initialization of a window.
        if (window.pairApi) {
            unsubs.push(
                window.pairApi.workers.onUpdate((workers: WorkerStatus[]) => {
                    if (lifetime !== generation) return
                    revision++
                    set({ workers })
                })
            )
        }
        await get().refresh()
    },

    cleanup: () => {
        generation++
        unsubs.forEach(u => u())
        unsubs = []
        // A resubscribe must not keep the previous connection's rows on screen.
        set({ workers: [] })
    }
}))
