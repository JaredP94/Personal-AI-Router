// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { gatewayEndpointDisplayUrl } from '@/ui/utils/gateway-inference-paths'

describe('copied inference base URLs', () => {
    it('keeps the native Ollama API root unversioned', () => {
        expect(gatewayEndpointDisplayUrl(54301, 'ollama')).toBe('http://127.0.0.1:54301')
    })

    it.each(['lm-studio', 'omlx'] as const)('includes /v1 for %s OpenAI clients', engine => {
        expect(gatewayEndpointDisplayUrl(54302, engine)).toBe('http://127.0.0.1:54302/v1')
    })

    it('does not fabricate a URL before the port is known', () => {
        expect(gatewayEndpointDisplayUrl(0, 'ollama')).toBeNull()
        expect(gatewayEndpointDisplayUrl(-1, 'omlx')).toBeNull()
    })
})
