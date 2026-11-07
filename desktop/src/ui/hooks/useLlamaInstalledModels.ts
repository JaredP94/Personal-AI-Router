// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
import { useCallback, useEffect, useRef, useState } from 'react'
import type { DownloadedModel } from '@/shared/types/engine-api'
import { useEngineProgressStore } from '@/ui/stores/engine-progress.store'
import { usePendingActionsStore } from '@/ui/stores/pending-actions.store'
import { useConnectionStore } from '@/ui/stores/connection.store'

export function useLlamaInstalledModels(nodeId: string, enabled: boolean) {
    const [models, setModels] = useState<DownloadedModel[]>([])
    const [error, setError] = useState('')
    const generation = useRef(0)
    const connected = useConnectionStore(state => state.connected)
    const progress = useEngineProgressStore(state =>
        [...state.progress.values()]
            .filter(p => p.nodeId === nodeId && p.engineType === 'llamacpp')
            .map(p => `${p.opId}:${p.status}`)
            .join('|')
    )
    const pending = usePendingActionsStore(state =>
        [...state.pending.keys()].filter(key => key.startsWith(`${nodeId}:llamacpp:`)).join('|')
    )
    const reload = useCallback(async () => {
        const current = ++generation.current
        if (!enabled || !connected) {
            setModels([])
            return
        }
        try {
            const result = await window.pairApi.engines.downloadedModels(nodeId)
            if (generation.current !== current) return
            setModels(result)
            setError('')
        } catch (cause) {
            if (generation.current !== current) return
            setModels([])
            setError(cause instanceof Error ? cause.message : 'Could not read installed models.')
        }
    }, [nodeId, enabled, connected])
    useEffect(() => {
        void reload()
        return invalidate
        function invalidate(): void {
            generation.current++
        }
    }, [reload, progress, pending])
    return { models, error, reload }
}
