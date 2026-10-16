// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import {
    JsonRpcResponseError,
    JsonRpcSubprocess,
    type JsonValue
} from '@/electron/service-bridge/json-rpc-subprocess'

const FAKE_RPC = `
const readline = require('readline').createInterface({ input: process.stdin })
readline.on('line', line => {
  const request = JSON.parse(line)
  const response = request.method === 'reject'
    ? { jsonrpc: '2.0', id: request.id, error: { code: -32000, message: 'rejected' } }
    : { jsonrpc: '2.0', id: request.id, result: null }
  setTimeout(() => process.stdout.write(JSON.stringify(response) + '\\n'), 10)
})
`

// Exits as soon as it is asked to shut down, so `exit` has already fired by the
// time stop() starts waiting for it.
const FAKE_EARLY_EXIT = `
const readline = require('readline').createInterface({ input: process.stdin })
readline.on('line', () => process.exit(0))
`

// Echoes every frame it reads back as an `echo` notification whose params are the
// frame verbatim, so the test can inspect exactly what went down the pipe rather
// than infer it.
const FAKE_ECHO = `
const readline = require('readline').createInterface({ input: process.stdin })
readline.on('line', line => {
  const frame = JSON.parse(line)
  // Every frame comes back as an echo notification; a request is additionally
  // answered so a caller waiting on it is not left hanging.
  process.stdout.write(JSON.stringify({ jsonrpc: '2.0', method: 'echo', params: frame }) + '\\n')
  if (frame.id !== undefined) {
    process.stdout.write(JSON.stringify({ jsonrpc: '2.0', id: frame.id, result: null }) + '\\n')
  }
})
`

function isJsonObject(value: JsonValue | undefined): value is Record<string, JsonValue> {
    return typeof value === 'object' && value !== null && !Array.isArray(value)
}

describe('JsonRpcSubprocess.sendWithResponse', () => {
    it('retains numeric error codes and response-error identity without changing the message', async () => {
        const rpc = new JsonRpcSubprocess('fake', process.execPath)
        rpc.start(['-e', FAKE_RPC])
        try {
            let responseError: JsonRpcResponseError | undefined
            try {
                await rpc.call('reject')
            } catch (error) {
                if (!(error instanceof JsonRpcResponseError)) throw error
                responseError = error
            }
            expect(responseError).toBeInstanceOf(JsonRpcResponseError)
            expect(responseError?.code).toBe(-32000)
            expect(responseError?.message).toBe('-32000: rejected')
        } finally {
            await rpc.stop()
        }
    })

    it('observes delayed responses without imposing a short timeout', async () => {
        const rpc = new JsonRpcSubprocess('fake', process.execPath)
        rpc.start(['-e', FAKE_RPC])
        try {
            await expect(rpc.sendWithResponse('reject')).rejects.toThrow('-32000: rejected')
            await expect(rpc.sendWithResponse('accept')).resolves.toBeUndefined()
        } finally {
            await rpc.stop()
        }
    })
})

describe('JsonRpcSubprocess.stop', () => {
    // A child that dies during the shutdown call leaves no 'exit' left to observe,
    // so a listener attached afterwards never fires: the wait then burns the whole
    // grace and reports the delay as a blocked main process, blaming the wrong
    // side for a child that simply left early.
    it('returns promptly when the child exits while the shutdown call is in flight', async () => {
        const rpc = new JsonRpcSubprocess('fake', process.execPath)
        rpc.start(['-e', FAKE_EARLY_EXIT])

        const startedAt = Date.now()
        await rpc.stop(15_000)

        expect(Date.now() - startedAt).toBeLessThan(4_000)
    })

    it('returns promptly when the child is already gone', async () => {
        const rpc = new JsonRpcSubprocess('fake', process.execPath)
        rpc.start(['-e', FAKE_EARLY_EXIT])

        const exited = new Promise<void>(resolve => rpc.once('exit', () => resolve()))
        await rpc.notify('anything')
        await exited

        const startedAt = Date.now()
        await rpc.stop(15_000)

        expect(Date.now() - startedAt).toBeLessThan(2_000)
        expect(rpc.running).toBe(false)
    })
})

describe('JsonRpcSubprocess.notify', () => {
    // Protocol v2: a Go peer reads this stream with the shared codec, whose
    // validate() treats a notification without epoch/seq as a terminal protocol
    // mismatch — a broken pipe for the peer, not a malformed frame. This is the
    // last emitter on the desktop side of that wire (modular-supervisor reports
    // errors:report and log/set-level through it), so the stamp has to be here.
    it('stamps every notification with a non-empty epoch and a monotonic seq', async () => {
        const rpc = new JsonRpcSubprocess('fake', process.execPath)
        rpc.start(['-e', FAKE_ECHO])
        try {
            const frames: Record<string, JsonValue>[] = []
            const collected = new Promise<void>(resolve => {
                rpc.on('notification', note => {
                    if (note.method !== 'echo') return
                    if (!isJsonObject(note.params)) return
                    frames.push(note.params)
                    if (frames.length === 5) resolve()
                })
            })

            for (let i = 0; i < 5; i++) {
                await rpc.notify('errors:report', { i })
            }
            await collected

            expect(frames).toHaveLength(5)
            const epochs = new Set<string>()
            frames.forEach((frame, index) => {
                expect(typeof frame.epoch, `frame ${index} epoch`).toBe('string')
                expect(String(frame.epoch).length).toBeGreaterThan(0)
                epochs.add(String(frame.epoch))
                // seq starts at 1 for this subprocess and increases by one per
                // notification, preserving the sender's wire-order guarantee.
                // Gaps/regressions do not trigger receiver resync.
                expect(frame.seq).toBe(index + 1)
            })
            // One subprocess is one epoch, so the peer can tell a restart from
            // a gap.
            expect(epochs.size).toBe(1)
        } finally {
            await rpc.stop()
        }
    })

    it('leaves requests unstamped, because a stamp on a non-notification is the same violation', async () => {
        const rpc = new JsonRpcSubprocess('fake', process.execPath)
        rpc.start(['-e', FAKE_ECHO])
        try {
            const echoed = new Promise<Record<string, JsonValue>>(resolve => {
                rpc.on('notification', note => {
                    if (note.method === 'echo' && isJsonObject(note.params)) resolve(note.params)
                })
            })

            const result = rpc.call('errors:get-initial')
            const frame = await echoed
            await result

            expect(frame.id).toBeDefined()
            expect(frame.method).toBe('errors:get-initial')
            expect(frame.epoch).toBeUndefined()
            expect(frame.seq).toBeUndefined()
        } finally {
            await rpc.stop()
        }
    })

    it('parses epoch and seq off inbound notifications and adopts the peer epoch for subsequent notify() calls', async () => {
        const peerScript = `
const readline = require('readline').createInterface({ input: process.stdin })
readline.on('line', line => {
  const frame = JSON.parse(line)
  if (frame.method === 'trigger-peer-epoch') {
    process.stdout.write(JSON.stringify({
      jsonrpc: '2.0',
      method: 'heartbeat',
      params: {},
      epoch: 'broker-epoch-v2',
      seq: 7
    }) + '\\n')
    return
  }
  process.stdout.write(JSON.stringify({ jsonrpc: '2.0', method: 'echo', params: frame }) + '\\n')
})
`
        const rpc = new JsonRpcSubprocess('fake', process.execPath)
        rpc.start(['-e', peerScript])
        try {
            const firstEcho = new Promise<Record<string, JsonValue>>(resolve => {
                const onNote = (note: { method: string; params?: JsonValue }): void => {
                    if (note.method === 'echo' && isJsonObject(note.params)) {
                        rpc.off('notification', onNote)
                        resolve(note.params)
                    }
                }
                rpc.on('notification', onNote)
            })
            await rpc.notify('before-peer-epoch')
            const beforeFrame = await firstEcho
            expect(typeof beforeFrame.epoch).toBe('string')
            expect(beforeFrame.epoch).not.toBe('broker-epoch-v2')
            expect(beforeFrame.seq).toBe(1)

            const peerHeartbeat = new Promise<{
                method: string
                epoch?: string
                seq?: number
            }>(resolve => {
                const onNote = (note: { method: string; epoch?: string; seq?: number }): void => {
                    if (note.method === 'heartbeat') {
                        rpc.off('notification', onNote)
                        resolve(note)
                    }
                }
                rpc.on('notification', onNote)
            })
            await rpc.notify('trigger-peer-epoch')
            const heartbeatNote = await peerHeartbeat
            expect(heartbeatNote.epoch).toBe('broker-epoch-v2')
            expect(heartbeatNote.seq).toBe(7)

            const secondEcho = new Promise<Record<string, JsonValue>>(resolve => {
                const onNote = (note: { method: string; params?: JsonValue }): void => {
                    if (note.method === 'echo' && isJsonObject(note.params)) {
                        rpc.off('notification', onNote)
                        resolve(note.params)
                    }
                }
                rpc.on('notification', onNote)
            })
            await rpc.notify('after-peer-epoch')
            const afterFrame = await secondEcho
            expect(afterFrame.epoch).toBe('broker-epoch-v2')
            expect(afterFrame.seq).toBe(3)
        } finally {
            await rpc.stop()
        }
    })
})
