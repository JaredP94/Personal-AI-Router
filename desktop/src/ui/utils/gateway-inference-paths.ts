// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { EngineType } from '@/shared/types/engines'
import { MODULAR_OPENAI_API_BASE_PATH } from '@/shared/constants/modular-runtime'

/** Native Ollama root or OpenAI-compatible base URL for the other engines. */
export function gatewayEndpointDisplayUrl(
    proxyPort: number,
    inferenceType: EngineType
): string | null {
    if (proxyPort <= 0) return null
    const path = inferenceType === 'ollama' ? '' : MODULAR_OPENAI_API_BASE_PATH
    return `http://127.0.0.1:${proxyPort}${path}`
}

export function gatewayEndpointApiLabel(inferenceType: EngineType): string {
    return inferenceType === 'ollama' ? 'Ollama native API' : 'OpenAI-compatible API'
}
