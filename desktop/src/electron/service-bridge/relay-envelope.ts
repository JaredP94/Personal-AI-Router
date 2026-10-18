// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import getErrorString from '@/shared/utils/get-error-string'
import type { JsonObject, JsonRpcError, JsonValue } from './json-rpc-subprocess'

interface JsonParseReviverContext {
    readonly source?: string
}

declare global {
    interface JSON {
        rawJSON(text: string): object
        parse(
            text: string,
            reviver: (
                this: object,
                key: string,
                value: JsonValue,
                context: JsonParseReviverContext
            ) => JsonValue
        ): JsonValue
    }
}

export interface JsonRpcMessage {
    jsonrpc: '2.0'
    id?: number | string | null
    method?: string
    params?: JsonValue
    result?: JsonValue
    error?: JsonRpcError
    epoch?: string
    seq?: bigint
}

export type RelayFrame =
    | { kind: 'message'; message: JsonRpcMessage; epoch?: string; seq?: bigint }
    | { kind: 'decode-error'; reason: string }
    | { kind: 'mismatch'; reason: string }

const MAX_UINT64 = 18446744073709551615n
const SEQ_DECIMAL_PATTERN = /^(0|[1-9][0-9]*)$/
const RESERVED_MEMBERS = ['jsonrpc', 'id', 'method', 'params', 'result', 'error', 'epoch', 'seq']

function assertRuntimeCapabilities(): void {
    if (typeof JSON.rawJSON !== 'function') {
        throw new Error('Runtime does not support JSON.rawJSON')
    }
    let reviverSourceSupported = false
    JSON.parse('{"probe":1}', (_key, value, context) => {
        if (context.source === '1') {
            reviverSourceSupported = true
        }
        return value
    })
    if (!reviverSourceSupported) {
        throw new Error('Runtime does not support JSON.parse reviver context.source')
    }
}

assertRuntimeCapabilities()

export function encodeSeq(seq: bigint): string {
    if (seq < 0n || seq > MAX_UINT64) {
        throw new RangeError(`seq out of uint64 range: ${seq}`)
    }
    const raw = JSON.rawJSON(seq.toString())
    return JSON.stringify(raw)
}

function isWhitespace(ch: string): boolean {
    return ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r'
}

function isHexDigit(ch: string): boolean {
    return (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')
}

function decodeJsonString(
    source: string,
    startIndex: number
): { readonly value: string; readonly nextIndex: number } | null {
    if (source[startIndex] !== '"') {
        return null
    }
    let i = startIndex + 1
    let result = ''
    while (i < source.length) {
        const ch = source[i]
        if (ch === '"') {
            return { value: result, nextIndex: i + 1 }
        }
        if (ch === '\\') {
            i++
            if (i >= source.length) {
                return null
            }
            const esc = source[i]
            if (esc === '"' || esc === '\\' || esc === '/') {
                result += esc
                i++
            } else if (esc === 'b') {
                result += '\b'
                i++
            } else if (esc === 'f') {
                result += '\f'
                i++
            } else if (esc === 'n') {
                result += '\n'
                i++
            } else if (esc === 'r') {
                result += '\r'
                i++
            } else if (esc === 't') {
                result += '\t'
                i++
            } else if (esc === 'u') {
                if (i + 4 >= source.length) {
                    return null
                }
                const d0 = source[i + 1]
                const d1 = source[i + 2]
                const d2 = source[i + 3]
                const d3 = source[i + 4]
                if (!isHexDigit(d0) || !isHexDigit(d1) || !isHexDigit(d2) || !isHexDigit(d3)) {
                    return null
                }
                const hex = source.slice(i + 1, i + 5)
                const code = parseInt(hex, 16)
                result += String.fromCharCode(code)
                i += 5
            } else {
                return null
            }
        } else {
            if (ch < ' ') {
                return null
            }
            result += ch
            i++
        }
    }
    return null
}

interface CanonicalCheckResult {
    readonly ok: boolean
    readonly reason?: string
}

function checkCanonical(line: string): CanonicalCheckResult {
    let i = 0
    while (i < line.length && isWhitespace(line[i])) {
        i++
    }
    if (i >= line.length || line[i] !== '{') {
        return { ok: true }
    }
    i++

    const seen = new Set<string>()

    while (i < line.length) {
        while (i < line.length && isWhitespace(line[i])) {
            i++
        }
        if (i >= line.length) {
            return { ok: true }
        }
        if (line[i] === '}') {
            return { ok: true }
        }
        if (line[i] !== '"') {
            return { ok: true }
        }
        const keyResult = decodeJsonString(line, i)
        if (!keyResult) {
            return { ok: true }
        }
        const key = keyResult.value
        i = keyResult.nextIndex

        const keyLower = key.toLowerCase()
        for (const reserved of RESERVED_MEMBERS) {
            if (keyLower === reserved) {
                if (key !== reserved) {
                    return {
                        ok: false,
                        reason: `non-canonical envelope member "${key}" (must be "${reserved}")`
                    }
                }
                if (seen.has(reserved)) {
                    return {
                        ok: false,
                        reason: `duplicate envelope member "${reserved}"`
                    }
                }
                seen.add(reserved)
                break
            }
        }

        while (i < line.length && isWhitespace(line[i])) {
            i++
        }
        if (i >= line.length || line[i] !== ':') {
            return { ok: true }
        }
        i++

        while (i < line.length && isWhitespace(line[i])) {
            i++
        }
        if (i >= line.length) {
            return { ok: true }
        }

        if (line[i] === '"') {
            const strVal = decodeJsonString(line, i)
            if (!strVal) {
                return { ok: true }
            }
            i = strVal.nextIndex
        } else if (line[i] === '{' || line[i] === '[') {
            let depth = 1
            i++
            while (i < line.length && depth > 0) {
                const ch = line[i]
                if (ch === '"') {
                    const strVal = decodeJsonString(line, i)
                    if (!strVal) {
                        return { ok: true }
                    }
                    i = strVal.nextIndex
                } else if (ch === '{' || ch === '[') {
                    depth++
                    i++
                } else if (ch === '}' || ch === ']') {
                    depth--
                    i++
                } else {
                    i++
                }
            }
        } else {
            while (i < line.length && line[i] !== ',' && line[i] !== '}') {
                i++
            }
        }

        while (i < line.length && isWhitespace(line[i])) {
            i++
        }
        if (i >= line.length) {
            return { ok: true }
        }
        if (line[i] === ',') {
            i++
        } else if (line[i] === '}') {
            return { ok: true }
        } else {
            return { ok: true }
        }
    }
    return { ok: true }
}

function isJsonObject(value: JsonValue): value is JsonObject {
    return value !== null && typeof value === 'object' && !Array.isArray(value)
}

export function decodeRelayFrame(line: string): RelayFrame {
    const canonical = checkCanonical(line)
    if (!canonical.ok) {
        return { kind: 'decode-error', reason: canonical.reason ?? 'non-canonical frame' }
    }

    const seqSources = new WeakMap<object, string>()
    let rootValue: JsonValue
    try {
        rootValue = JSON.parse(
            line,
            function (
                this: object,
                key: string,
                value: JsonValue,
                context: JsonParseReviverContext
            ) {
                if (key === 'seq' && context.source !== undefined) {
                    seqSources.set(this, context.source)
                }
                return value
            }
        )
    } catch (err) {
        return { kind: 'decode-error', reason: `invalid JSON: ${getErrorString(err)}` }
    }

    if (!isJsonObject(rootValue)) {
        return { kind: 'decode-error', reason: 'message must be a JSON object' }
    }

    const jsonrpc = rootValue.jsonrpc
    if (jsonrpc !== '2.0') {
        return { kind: 'decode-error', reason: `unsupported JSON-RPC version: ${String(jsonrpc)}` }
    }

    const seqSource = seqSources.get(rootValue)
    let decodedSeq: bigint | undefined
    if (seqSource !== undefined && seqSource !== 'null') {
        if (!SEQ_DECIMAL_PATTERN.test(seqSource)) {
            return { kind: 'decode-error', reason: `invalid seq format: ${seqSource}` }
        }
        const parsed = BigInt(seqSource)
        if (parsed > MAX_UINT64) {
            return { kind: 'decode-error', reason: `seq exceeds uint64 max: ${seqSource}` }
        }
        decodedSeq = parsed
    }

    const epochValue = rootValue.epoch
    let decodedEpoch: string | undefined
    if (epochValue !== undefined && epochValue !== null) {
        if (typeof epochValue !== 'string') {
            return { kind: 'decode-error', reason: 'epoch must be a string' }
        }
        decodedEpoch = epochValue
    }

    const idValue = rootValue.id
    const isIdAbsent = idValue === undefined || idValue === null

    const methodValue = rootValue.method
    let messageMethod: string | undefined
    if (methodValue !== undefined && methodValue !== null) {
        if (typeof methodValue === 'string') {
            messageMethod = methodValue
        } else {
            return { kind: 'decode-error', reason: 'method must be a string' }
        }
    }

    const isNotification =
        isIdAbsent && typeof messageMethod === 'string' && messageMethod.length > 0

    if (isNotification) {
        if (decodedEpoch === undefined || decodedSeq === undefined) {
            return {
                kind: 'mismatch',
                reason: `notification without epoch/seq (method "${messageMethod}")`
            }
        }
    } else {
        if (decodedEpoch !== undefined || decodedSeq !== undefined) {
            const methodStr = typeof messageMethod === 'string' ? messageMethod : ''
            return {
                kind: 'mismatch',
                reason: `epoch/seq on a non-notification frame (method "${methodStr}")`
            }
        }
    }

    let messageId: number | string | null | undefined
    if (!isIdAbsent) {
        if (typeof idValue === 'number' || typeof idValue === 'string') {
            messageId = idValue
        } else {
            return { kind: 'decode-error', reason: 'id must be a number, string, or null' }
        }
    }

    let errorObj: JsonRpcError | undefined
    const rawError = rootValue.error
    if (rawError !== undefined && rawError !== null) {
        if (
            isJsonObject(rawError) &&
            typeof rawError.code === 'number' &&
            typeof rawError.message === 'string'
        ) {
            errorObj = {
                code: rawError.code,
                message: rawError.message,
                ...(rawError.data !== undefined ? { data: rawError.data } : {})
            }
        } else {
            return { kind: 'decode-error', reason: 'invalid error object' }
        }
    }

    const message: JsonRpcMessage = {
        jsonrpc: '2.0',
        ...(messageId !== undefined ? { id: messageId } : {}),
        ...(messageMethod !== undefined ? { method: messageMethod } : {}),
        ...(rootValue.params !== undefined ? { params: rootValue.params } : {}),
        ...(rootValue.result !== undefined ? { result: rootValue.result } : {}),
        ...(errorObj !== undefined ? { error: errorObj } : {}),
        ...(decodedEpoch !== undefined ? { epoch: decodedEpoch } : {}),
        ...(decodedSeq !== undefined ? { seq: decodedSeq } : {})
    }

    return {
        kind: 'message',
        message,
        ...(decodedEpoch !== undefined ? { epoch: decodedEpoch } : {}),
        ...(decodedSeq !== undefined ? { seq: decodedSeq } : {})
    }
}
