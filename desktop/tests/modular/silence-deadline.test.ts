// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'
import { MODULAR_HEARTBEAT_TIMEOUT_MS } from '@/shared/constants/modular-runtime'
import { JsonRpcSubprocess } from '@/electron/service-bridge/json-rpc-subprocess'

const FAKE = `
const reader = require('readline').createInterface({ input: process.stdin })
reader.on('line', line => {
 const frame = JSON.parse(line)
 process.stdout.write(JSON.stringify({jsonrpc:'2.0',method:'heartbeat',params:{},epoch:'fake',seq:1})+'\\n')
 if (frame.id !== undefined && frame.method !== 'held') {
  process.stdout.write(JSON.stringify({jsonrpc:'2.0',id:frame.id,result:null})+'\\n')
 }
})
`

describe('inbound silence deadline', () => {
    it('expires once and rejects pending calls on an open transport', async () => {
        vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
        const rpc = new JsonRpcSubprocess('fake', process.execPath)
        const expired = vi.fn()
        rpc.on('unresponsive', expired)
        rpc.start(['-e', FAKE])
        try {
            const received = new Promise<void>(resolve => rpc.once('notification', () => resolve()))
            const pending = rpc.call('held', undefined, null).catch(error => error)
            await received
            await vi.advanceTimersByTimeAsync(MODULAR_HEARTBEAT_TIMEOUT_MS + 1)
            expect(expired).toHaveBeenCalledTimes(1)
            expect(await Promise.race([pending, Promise.resolve('still pending')])).toBeInstanceOf(
                Error
            )
            await vi.advanceTimersByTimeAsync(MODULAR_HEARTBEAT_TIMEOUT_MS)
            expect(expired).toHaveBeenCalledTimes(1)
        } finally {
            vi.useRealTimers()
            await rpc.stop()
        }
    })

    it('healthy frames reset the deadline and teardown cancels old lifetime timers', async () => {
        vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
        const rpc = new JsonRpcSubprocess('fake', process.execPath)
        const expired = vi.fn()
        rpc.on('unresponsive', expired)
        rpc.start(['-e', FAKE])
        try {
            for (let i = 0; i < 5; i++) {
                const received = new Promise<void>(resolve =>
                    rpc.once('notification', () => resolve())
                )
                await rpc.notify('fixture')
                await received
                await vi.advanceTimersByTimeAsync(15_000)
                expect(expired).not.toHaveBeenCalled()
            }
            await rpc.stop()
            await vi.advanceTimersByTimeAsync(MODULAR_HEARTBEAT_TIMEOUT_MS * 2)
            expect(expired).not.toHaveBeenCalled()
            rpc.start(['-e', FAKE])
            const received = new Promise<void>(resolve => rpc.once('notification', () => resolve()))
            await rpc.notify('new-lifetime')
            await received
            await vi.advanceTimersByTimeAsync(15_000)
            expect(expired).not.toHaveBeenCalled()
        } finally {
            vi.useRealTimers()
            await rpc.stop()
        }
    })
})
