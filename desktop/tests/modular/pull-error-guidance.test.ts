// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { pullErrorGuidance } from '@/ui/utils/pull-error-guidance'

function guidance(message: string): string | null {
    return pullErrorGuidance({ id: 'pull-failed', timestamp: 1, operation: 'pull', message })
}

describe('pull error guidance', () => {
    it.each([
        'no space left on device',
        'ENOSPC',
        'disk full',
        'There is not enough space on the disk'
    ])('recognises disk exhaustion: %s', message => {
        expect(guidance(message)).toContain('Free disk space')
    })

    it.each([
        'lookup registry.example: no such host',
        'getaddrinfo ENOTFOUND registry.example',
        'EAI_AGAIN',
        'temporary failure in name resolution'
    ])('recognises DNS errors: %s', message => {
        expect(guidance(message)).toContain('Check DNS resolution')
    })

    it.each([
        'read tcp: i/o timeout',
        'connect ETIMEDOUT',
        'connection timed out',
        'net/http: request canceled (Client.Timeout exceeded while awaiting headers)'
    ])('recognises network timeouts: %s', message => {
        expect(guidance(message)).toContain('network request timed out')
    })

    it.each([
        'x509: certificate signed by unknown authority',
        'certificate verify failed',
        'net/http: TLS handshake timeout',
        'SSL certificate problem: certificate has expired'
    ])('recognises TLS errors without recommending bypass: %s', message => {
        expect(guidance(message)).toContain('Do not disable certificate verification.')
    })

    it('identifies IPv6 only when an address and reachability failure are present', () => {
        expect(guidance('dial tcp [2001:db8::1]:443: connect: network is unreachable')).toContain(
            'IPv6 address is unreachable'
        )
        expect(guidance('dial tcp [2001:db8::1]:443: i/o timeout')).not.toContain('IPv6')
        expect(guidance('dial tcp 192.0.2.1:443: network is unreachable')).toBeNull()
    })

    it.each([
        'model demo:latest not found',
        'Repository Not Found',
        'pull model manifest: file does not exist'
    ])('recognises missing models: %s', message => {
        expect(guidance(message)).toContain('Check the model name, tag, and availability')
    })

    it.each([
        'unauthorized: authentication required',
        'HTTP 401 Unauthorized',
        'HTTP status code: 403',
        'invalid access token'
    ])('recognises access errors: %s', message => {
        expect(guidance(message)).toContain('Check access to the model')
        expect(guidance(message)).toContain('Do not share tokens or credentials.')
    })

    it.each([
        'download failed',
        'exit status 1',
        'context canceled',
        'context deadline exceeded',
        'HTTP 500',
        'unexpected HTTP 404',
        'file not found',
        'permission denied',
        'model 401',
        'pull "unauthorized": unexpected EOF',
        'pull "ENOSPC": unexpected EOF',
        'Get "https://registry.example/unauthorized": EOF',
        'Get https://registry.example/unauthorized: EOF'
    ])('does not guess from ambiguous errors: %s', message => {
        expect(guidance(message)).toBeNull()
    })

    it('leaves unrelated operations and the raw message untouched', () => {
        const error = { id: 'failure', timestamp: 1, operation: 'install', message: 'ENOSPC' }
        expect(pullErrorGuidance(error)).toBeNull()
        expect(error.message).toBe('ENOSPC')
    })
})
