// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// @vitest-environment jsdom

import { act, createElement } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { BackendHeader } from '@/ui/components/BackendRow/BackendHeader'
import EndpointContent from '@/ui/components/EndpointContent'
import { useEngineStatusStore } from '@/ui/stores/engine-status.store'
import { EngineDisplayNames, EngineTypes } from '@/shared/constants/engines'
import { noEngineCapabilities } from '@/shared/utils/engines'

let container: HTMLDivElement
let root: Root
const copyToClipboard = vi.fn<(value: string) => Promise<void>>()

describe('endpoint copy actions', () => {
    beforeEach(() => {
        vi.useFakeTimers()
        vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true)
        copyToClipboard.mockResolvedValue(undefined)
        vi.stubGlobal('windowApi', { window: { copyToClipboard } })
        container = document.createElement('div')
        document.body.appendChild(container)
        root = createRoot(container)
    })
    afterEach(() => {
        act(() => root.unmount())
        container.remove()
        useEngineStatusStore.setState({ statusByNode: new Map() })
        vi.clearAllTimers()
        vi.useRealTimers()
        vi.unstubAllGlobals()
    })

    for (const engine of EngineTypes) {
        const name = EngineDisplayNames[engine]
        const apiLabel = engine === 'ollama' ? 'Ollama native API' : 'OpenAI-compatible API'
        const url = `http://127.0.0.1:54301${engine === 'ollama' ? '' : '/v1'}`

        for (const surface of ['header', 'endpoints']) {
            it(`${surface} labels and copies the ${engine} API base`, async () => {
                act(() => {
                    if (surface === 'header') {
                        root.render(
                            createElement(BackendHeader, {
                                backend: {
                                    type: engine,
                                    displayName: name,
                                    source: 'installed',
                                    processStatus: 'running',
                                    port: 1235,
                                    proxyPort: 54301,
                                    models: []
                                },
                                isTransitioning: false,
                                isUnavailable: false,
                                disabled: false,
                                onToggle: vi.fn(),
                                onExpand: vi.fn(),
                                expanded: false,
                                isLocalNode: true,
                                targetOs: 'MacOS',
                                canInstall: false,
                                onInstall: vi.fn()
                            })
                        )
                    } else {
                        useEngineStatusStore.setState({
                            statusByNode: new Map([
                                [
                                    'local',
                                    new Map([
                                        [
                                            engine,
                                            {
                                                engineType: engine,
                                                nodeId: 'local',
                                                processStatus: 'running',
                                                enginePort: 1235,
                                                proxyPort: 54301,
                                                capabilities: noEngineCapabilities()
                                            }
                                        ]
                                    ])
                                ]
                            ])
                        })
                        root.render(createElement(EndpointContent, { selfId: 'local' }))
                    }
                })
                const button = Array.from(container.querySelectorAll('button')).find(
                    element => element.getAttribute('aria-label') === `Copy ${name} ${apiLabel} URL`
                )
                expect(button).toBeDefined()
                if (surface === 'header') expect(button?.title).toContain(url)
                else expect(container.textContent).toContain(url)
                await act(async () => {
                    button?.click()
                })
                expect(copyToClipboard).toHaveBeenCalledExactlyOnceWith(url)
            })
        }
    }
})
