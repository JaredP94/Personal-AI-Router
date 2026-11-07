// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// @vitest-environment jsdom
import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'
import { expect, it, vi } from 'vitest'
import { ModelHubContent } from '@/ui/components/ModelHub/ModelHubContent'

it('browses llama.cpp by publisher and shows bounded service search errors calmly', async () => {
    vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true)
    vi.stubGlobal(
        'ResizeObserver',
        class {
            observe() {
                /* jsdom has no layout observer */
            }
            disconnect() {
                /* no observer resources */
            }
        }
    )
    vi.useFakeTimers()
    const searchHub = vi.fn(async (_engine: string, query?: string) => {
        if (query) throw new Error('query must not exceed 100 characters')
        return {
            models: ['unsloth/Demo:Q4_K_M', 'ggml-org/Small:Q4_K_M'].map(id => ({
                id,
                name: id,
                author: id.split('/')[0],
                url: '',
                downloads: 0,
                likes: 0,
                updatedAt: '',
                tags: []
            }))
        }
    })
    vi.stubGlobal('pairApi', { engines: { searchHub } })
    const container = document.createElement('div')
    document.body.append(container)
    const root = createRoot(container)
    try {
        await act(async () =>
            root.render(
                createElement(ModelHubContent, {
                    engine: 'llamacpp',
                    multiple: false,
                    onSubmit: () => {}
                })
            )
        )
        expect(container.textContent).toContain('ggml-org')
        expect(container.textContent).toContain('unsloth')
        expect(container.textContent).toContain('unsloth/Demo:Q4_K_M')
        await act(async () =>
            root.render(
                createElement(ModelHubContent, {
                    engine: null,
                    multiple: false,
                    onSubmit: () => {}
                })
            )
        )
        await act(async () =>
            root.render(
                createElement(ModelHubContent, {
                    engine: 'llamacpp',
                    multiple: false,
                    onSubmit: () => {}
                })
            )
        )
        expect(searchHub.mock.calls.filter(call => !call[1])).toHaveLength(2)
        const input = container.querySelector<HTMLInputElement>('input')
        await act(async () => {
            Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')?.set?.call(
                input,
                'x'.repeat(101)
            )
            input?.dispatchEvent(new Event('input', { bubbles: true }))
        })
        await act(async () => {
            await vi.advanceTimersByTimeAsync(450)
        })
        expect(container.textContent).toContain('query must not exceed 100 characters')
        expect(container.querySelector('[role="alert"]')?.textContent).toContain('100 characters')
    } finally {
        act(() => root.unmount())
        container.remove()
        vi.useRealTimers()
        vi.unstubAllGlobals()
    }
})
