// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import {
    decodeRelayFrame,
    encodeSeq,
    type RelayFrame
} from '@/electron/service-bridge/relay-envelope'
import type { JsonObject, JsonValue } from '@/electron/service-bridge/json-rpc-subprocess'

interface ConformanceRow {
    readonly name: string
    readonly frame: string
    readonly expect: 'accept' | 'decode-error' | 'mismatch'
    readonly kind: 'notification' | 'request' | 'response' | null
    readonly epoch: string | null
    readonly seq: string | null
}

function isJsonObject(value: JsonValue): value is JsonObject {
    return value !== null && typeof value === 'object' && !Array.isArray(value)
}

function parseConformanceRow(data: JsonValue): ConformanceRow {
    if (!isJsonObject(data)) {
        throw new Error('Conformance row must be an object')
    }
    const name = typeof data.name === 'string' ? data.name : ''
    const frame = typeof data.frame === 'string' ? data.frame : ''
    const expectVal =
        data.expect === 'accept' || data.expect === 'decode-error' || data.expect === 'mismatch'
            ? data.expect
            : 'decode-error'
    const kindVal =
        data.kind === 'notification' || data.kind === 'request' || data.kind === 'response'
            ? data.kind
            : null
    const epochVal = typeof data.epoch === 'string' ? data.epoch : null
    const seqVal = typeof data.seq === 'string' ? data.seq : null
    return {
        name,
        frame,
        expect: expectVal,
        kind: kindVal,
        epoch: epochVal,
        seq: seqVal
    }
}

const corpusPath = resolve(
    __dirname,
    '../../../services/shared/jsonrpc/testdata/envelope-conformance.jsonl'
)
const corpusLines = readFileSync(corpusPath, 'utf8')
    .split('\n')
    .filter(line => line.trim().length > 0)
const corpusRows: ConformanceRow[] = corpusLines.map(line => {
    const parsed = JSON.parse(line)
    if (!isJsonObject(parsed)) {
        throw new Error('Expected JSON object in corpus')
    }
    return parseConformanceRow(parsed)
})

describe('relay envelope conformance corpus', () => {
    it.each(corpusRows)('corpus row $name: expect $expect', row => {
        const result: RelayFrame = decodeRelayFrame(row.frame)
        if (row.expect === 'accept') {
            expect(result.kind).toBe('message')
            if (result.kind === 'message') {
                if (row.kind === 'notification') {
                    const isNotification =
                        (result.message.id === undefined || result.message.id === null) &&
                        typeof result.message.method === 'string' &&
                        result.message.method.length > 0
                    expect(isNotification).toBe(true)
                } else if (row.kind === 'request') {
                    const isRequest =
                        result.message.id !== undefined &&
                        result.message.id !== null &&
                        typeof result.message.method === 'string' &&
                        result.message.method.length > 0
                    expect(isRequest).toBe(true)
                } else if (row.kind === 'response') {
                    const isResponse =
                        result.message.id !== undefined &&
                        result.message.id !== null &&
                        (result.message.method === undefined || result.message.method === '')
                    expect(isResponse).toBe(true)
                }
                const actualEpoch = result.epoch ?? null
                expect(actualEpoch).toBe(row.epoch)
                const actualSeq = result.seq !== undefined ? result.seq.toString() : null
                expect(actualSeq).toBe(row.seq)
            }
        } else if (row.expect === 'decode-error') {
            expect(result.kind).toBe('decode-error')
        } else if (row.expect === 'mismatch') {
            expect(result.kind).toBe('mismatch')
        }
    })
})

describe('encodeSeq and decodeRelayFrame roundtrip fuzz', () => {
    it('losslessly encodes and decodes 500 conforming notifications with random large seq values', () => {
        for (let i = 0; i < 500; i++) {
            const high = BigInt(Math.floor(Math.random() * 0x100000000))
            const low = BigInt(Math.floor(Math.random() * 0x100000000))
            const seq = (high << 32n) | low
            const epoch = `epoch-${i}-${Math.random().toString(36).slice(2)}`
            const method = `test:method_${i}`
            const line = `{"jsonrpc":"2.0","method":${JSON.stringify(method)},"epoch":${JSON.stringify(epoch)},"seq":${encodeSeq(seq)}}`

            const frame = decodeRelayFrame(line)
            expect(frame.kind).toBe('message')
            if (frame.kind === 'message') {
                expect(frame.epoch).toBe(epoch)
                expect(frame.seq).toBe(seq)
                expect(frame.message.epoch).toBe(epoch)
                expect(frame.message.seq).toBe(seq)
                expect(frame.message.method).toBe(method)
            }
        }
    })
})

describe('encodeSeq boundaries', () => {
    it('encodes 0 and max uint64 exactly', () => {
        expect(encodeSeq(0n)).toBe('0')
        expect(encodeSeq(18446744073709551615n)).toBe('18446744073709551615')
    })

    it('rejects negative seq and seq > 2^64 - 1', () => {
        expect(() => encodeSeq(-1n)).toThrow(RangeError)
        expect(() => encodeSeq(18446744073709551616n)).toThrow(RangeError)
    })
})

describe('runtime capability assertion', () => {
    it('confirms the runtime environment supports reviver context.source and JSON.rawJSON', () => {
        expect(typeof JSON.rawJSON).toBe('function')
        let sourceSeen = false
        JSON.parse('{"test":1}', (_key, val, ctx) => {
            if (ctx?.source === '1') {
                sourceSeen = true
            }
            return val
        })
        expect(sourceSeen).toBe(true)
    })
})
