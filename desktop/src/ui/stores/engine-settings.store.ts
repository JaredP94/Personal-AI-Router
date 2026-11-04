// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { create } from 'zustand'
import type {
    EngineSettingsRequest,
    EngineSettingsSnapshot,
    EngineSettingsTarget
} from '@/shared/types/engine-settings'

export const engineSettingsKey = (target: EngineSettingsTarget): string =>
    `${target.nodeId}:${target.engine}`

interface SettingsEntry {
    snapshot?: EngineSettingsSnapshot
    unavailable?: string
    pending?: string
    arrival: number
}
interface SettingsStore {
    entries: Record<string, SettingsEntry>
    receive: (snapshot: EngineSettingsSnapshot) => void
    disconnect: (nodeId?: string) => void
    reload: (target: EngineSettingsTarget) => Promise<void>
    apply: (request: EngineSettingsRequest) => Promise<void>
}

export const useEngineSettingsStore = create<SettingsStore>((set, get) => ({
    entries: {},
    receive: snapshot => {
        const key = engineSettingsKey(snapshot)
        const current = get().entries[key]
        if (
            current?.snapshot?.epoch === snapshot.epoch &&
            (current.snapshot.sequence > snapshot.sequence ||
                (current.snapshot.sequence === snapshot.sequence && !current.unavailable))
        )
            return
        set(state => ({
            entries: {
                ...state.entries,
                [key]: {
                    snapshot,
                    arrival: (current?.arrival ?? 0) + 1,
                    pending: current?.pending === snapshot.requestId ? undefined : current?.pending
                }
            }
        }))
    },
    disconnect: nodeId =>
        set(state => ({
            entries: Object.fromEntries(
                Object.entries(state.entries).map(([key, entry]) => [
                    key,
                    !nodeId || key.startsWith(`${nodeId}:`)
                        ? {
                              ...entry,
                              pending: undefined,
                              unavailable: 'Device disconnected. Reconnect to edit settings.',
                              arrival: entry.arrival + 1
                          }
                        : entry
                ])
            )
        })),
    reload: async target => {
        const key = engineSettingsKey(target)
        const arrival = (get().entries[key]?.arrival ?? 0) + 1
        // Register even the first read so a disconnect can invalidate it.
        set(state => ({ entries: { ...state.entries, [key]: { ...state.entries[key], arrival } } }))
        try {
            const snapshot = await window.pairApi.engines.getSettings(target)
            // A push, disconnect, or later read wins over this delayed reply.
            if (get().entries[key]?.arrival !== arrival) return
            get().receive({ ...snapshot, nodeId: target.nodeId })
        } catch {
            if (get().entries[key]?.arrival !== arrival) return
            set(state => ({
                entries: {
                    ...state.entries,
                    [key]: {
                        ...state.entries[key],
                        arrival,
                        pending: undefined,
                        unavailable:
                            'Settings unavailable. Reconnect or update the device and refresh.'
                    }
                }
            }))
        }
    },
    apply: async request => {
        const key = engineSettingsKey(request)
        set(state => ({
            entries: {
                ...state.entries,
                [key]: {
                    ...state.entries[key],
                    arrival: state.entries[key]?.arrival ?? 0,
                    pending: request.requestId
                }
            }
        }))
        let awaitingSnapshot = false
        try {
            const receipt = await window.pairApi.engines.applySettings(request)
            awaitingSnapshot = receipt.phase === 'applying'
            const snapshot = get().entries[key]?.snapshot
            // Receipts cannot manufacture durable state. Recover a missing
            // terminal push with a full read; applying receipts wait for push.
            if (
                receipt.phase !== 'applying' &&
                (!snapshot ||
                    snapshot.requestId !== request.requestId ||
                    snapshot.phase === 'applying')
            )
                await get().reload(request)
        } catch (error) {
            await get().reload(request)
            throw error
        } finally {
            if (!awaitingSnapshot)
                set(state => ({
                    entries: {
                        ...state.entries,
                        [key]: {
                            ...state.entries[key],
                            arrival: state.entries[key]?.arrival ?? 0,
                            pending:
                                state.entries[key]?.pending === request.requestId
                                    ? undefined
                                    : state.entries[key]?.pending
                        }
                    }
                }))
        }
    }
}))

let consumers = 0
let unsubscribe: Array<() => void> = []
/** Subscribe before fetching a baseline; open editors share the stream. */
export function watchEngineSettings(): () => void {
    if (consumers++ === 0)
        unsubscribe = [
            window.pairApi.engines.onSettingsChanged(snapshot =>
                useEngineSettingsStore.getState().receive(snapshot)
            ),
            window.pairApi.engines.onSettingsDisconnected(({ nodeId }) =>
                useEngineSettingsStore.getState().disconnect(nodeId)
            )
        ]
    return () => {
        if (--consumers === 0) {
            unsubscribe.forEach(stop => stop())
            unsubscribe = []
        }
    }
}
