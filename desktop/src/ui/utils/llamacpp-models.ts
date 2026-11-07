// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
import type { ModelEntry } from '@/ui/types/model-hub'

// This edits only the model option; Go remains the launch-text validator.
const modelOption = /(?:^|\s)(?:--model|-m)(?:=|\s+)(?:"[^"]*"|'[^']*'|[^\s]+)/g

export function groupCatalog(models: ModelEntry[]): Array<[string, ModelEntry[]]> {
    const groups = new Map<string, ModelEntry[]>()
    for (const model of models) {
        const publisher = model.author || model.id.split('/')[0]
        const rows = groups.get(publisher) ?? []
        rows.push(model)
        groups.set(publisher, rows)
    }
    return [...groups].sort(([a], [b]) => a.localeCompare(b))
}

export function selectedLlamaModel(text: string): string {
    const match = text.match(/(?:^|\s)(?:--model|-m)(?:=|\s+)("[^"]*"|'[^']*'|[^\s]+)/)
    const value = (match?.[1] ?? '').replace(/^["']|["']$/g, '')
    return value.startsWith('/dev/fd/') ? '' : value
}

export function selectLlamaModel(text: string, file: string): string {
    const rest = text.replace(modelOption, '').trim()
    return [rest, file ? `--model "${file.replace(/["\\]/g, '\\$&')}"` : '']
        .filter(Boolean)
        .join(' ')
}
