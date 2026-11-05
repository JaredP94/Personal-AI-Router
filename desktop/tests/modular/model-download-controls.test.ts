// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// @vitest-environment jsdom

import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'
import { expect, it, vi } from 'vitest'
import { IncomingSyncPullRow } from '@/ui/components/ModelManager/IncomingSyncPullRow'
import type { IncomingSyncRow } from '@/ui/types/model-manager'

it('renders progress, Canceling, confirmed cancellation and unsupported transfers', () => {
    vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true)
    const container = document.createElement('div')
    document.body.append(container)
    const root = createRoot(container)
    const cancel = vi.fn()
    const row: IncomingSyncRow = {
        rawModel: 'owner/model',
        label: 'Model',
        opId: 'current',
        canCancel: true,
        status: 'downloading',
        percent: 42
    }
    const render = (update: Partial<IncomingSyncRow>): void => {
        act(() =>
            root.render(
                createElement(IncomingSyncPullRow, { row: { ...row, ...update }, onCancel: cancel })
            )
        )
    }
    try {
        render({})
        expect(container.textContent).toContain('42%')
        const button = container.querySelector('button')
        expect(button?.textContent).toContain('Cancel')
        act(() => button?.click())
        expect(cancel).toHaveBeenCalledTimes(1)
        render({ status: 'canceling' })
        expect(container.textContent).toContain('Canceling')
        expect(container.textContent).not.toContain('42%')
        expect(container.querySelector('button')?.disabled).toBe(false)
        render({ status: 'cancelled', canCancel: false })
        expect(container.textContent).toContain('Partial data is kept so the download can resume.')
        expect(container.querySelector('button')).toBeNull()
        expect(container.querySelector('[role="status"]')).toBeNull()
        render({ canCancel: false })
        expect(container.querySelector('button')).toBeNull()
        expect(container.textContent).toContain('42%')
    } finally {
        act(() => root.unmount())
        container.remove()
        vi.unstubAllGlobals()
    }
})
