// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
import { describe, expect, it } from 'vitest'
import { groupCatalog, selectLlamaModel, selectedLlamaModel } from '@/ui/utils/llamacpp-models'
import { modelActionItems } from '@/ui/utils/engine-capability-gates'
import { EngineCapabilities } from '@/ui/constants/engine-capabilities'
import { noEngineCapabilities } from '@/shared/utils/engines'

describe('llama.cpp model management', () => {
    it('groups catalog IDs by publisher without changing model names', () => {
        const rows = ['unsloth/B:Q4_K_M', 'ggml-org/A:Q4_K_M', 'unsloth/C:Q4_K_M'].map(id => ({
            id,
            name: id,
            author: id.split('/')[0],
            url: ''
        }))
        expect(
            groupCatalog(rows).map(([publisher, models]) => [publisher, models.map(m => m.name)])
        ).toEqual([
            ['ggml-org', ['ggml-org/A:Q4_K_M']],
            ['unsloth', ['unsloth/B:Q4_K_M', 'unsloth/C:Q4_K_M']]
        ])
    })
    it('writes the cache-relative file while preserving inference arguments', () => {
        expect(selectLlamaModel('--ctx-size 4096 --model old.gguf --threads 4', 'new.gguf')).toBe(
            '--ctx-size 4096 --threads 4 --model "new.gguf"'
        )
        expect(selectedLlamaModel('--ctx-size 4096 --model "new.gguf"')).toBe('new.gguf')
        expect(selectLlamaModel('-m=old.gguf --threads 4', '')).toBe('--threads 4')
    })
    it('never retains a descriptor path as a selection', () => {
        expect(selectedLlamaModel('--model /dev/fd/3')).toBe('')
        expect(selectLlamaModel('--model /dev/fd/3', 'cache.gguf')).not.toContain('/dev/fd/')
    })
    it('disables deletion with a reason while llama.cpp is running and offers no Load', () => {
        const items = modelActionItems({
            capabilities: EngineCapabilities.llamacpp,
            reported: { ...noEngineCapabilities(), deleteModel: true },
            isRunning: true,
            isLoaded: false,
            downloaded: true,
            busy: false
        })
        expect(items).toEqual([
            { id: 'delete', children: 'Delete (stop engine first)', danger: true, disabled: true }
        ])
    })
})
