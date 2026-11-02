// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { ServiceError } from '@/shared/types/errors'

/** Presentation only: retain the engine's diagnostic and add no speculative cause. */
export function pullErrorGuidance(error: ServiceError): string | null {
    if (error.operation !== 'pull') return null
    // Quoted model names and request URLs are context, not diagnostic evidence.
    const message = error.message
        .replace(/"(?:[^"\\]|\\.)*"/g, '""')
        .replace(/https?:\/\/\S+/gi, '')

    if (
        /\b(?:ENOSPC|no space left on device|disk (?:is )?full|not enough space on (?:the )?disk)\b/i.test(
            message
        )
    ) {
        return 'Free disk space on the node running the engine, then retry.'
    }
    if (
        /\b(?:unauthorized|authentication required|invalid (?:access )?token)\b|\b(?:HTTP(?:\/\d(?:\.\d)?)?\s*(?:status(?: code)?[:=]?\s*)?|status(?: code)?[:=]?\s+)(?:401|403)\b/i.test(
            message
        )
    ) {
        return 'Check access to the model and sign in through the engine or model provider if required. Do not share tokens or credentials.'
    }
    if (
        /\b(?:model(?:\s+["'][^"'\n]+["']|\s+\S+)?\s+(?:was\s+)?not found|repository not found|pull model manifest: file does not exist)\b/i.test(
            message
        )
    ) {
        return 'Check the model name, tag, and availability at the model source, then retry.'
    }
    if (
        /\b(?:no such host|ENOTFOUND|EAI_AGAIN|temporary failure in name resolution)\b/i.test(
            message
        )
    ) {
        return 'Check DNS resolution and the network connection on the node running the engine, then retry.'
    }
    if (
        /\[[0-9a-f:.%]+:[0-9a-f:.%]*\]:\d+[^\n]*(?:network is unreachable|no route to host|ENETUNREACH|EHOSTUNREACH)/i.test(
            message
        )
    ) {
        return 'The reported IPv6 address is unreachable. Check IPv6 connectivity on the node running the engine or retry on a network with a working route.'
    }
    if (
        /\bx509:|\b(?:certificate verify failed|certificate verification failed|TLS handshake|SSL certificate problem)\b/i.test(
            message
        )
    ) {
        return 'Check the node’s clock, certificate trust, and any TLS-inspecting proxy. Do not disable certificate verification.'
    }
    if (
        /\b(?:i\/o timeout|ETIMEDOUT|connection timed out|net\/http: request canceled.*timeout|Client\.Timeout exceeded)\b/i.test(
            message
        )
    ) {
        return 'The network request timed out. Check connectivity and proxy settings on the node running the engine, then retry.'
    }
    return null
}
