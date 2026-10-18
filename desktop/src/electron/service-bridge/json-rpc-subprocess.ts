// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { spawn, type ChildProcessWithoutNullStreams } from 'child_process'
import { randomUUID } from 'crypto'
import readline from 'readline'
import { EventEmitter } from 'events'
import getErrorString from '@/shared/utils/get-error-string'
import { createStructuredLogger } from '@/shared/utils/log'
import { redactSensitiveLogText } from '@/electron/redact-log'
import { MODULAR_HEARTBEAT_TIMEOUT_MS } from '@/shared/constants/modular-runtime'
import { decodeRelayFrame, encodeSeq, type JsonRpcMessage } from './relay-envelope'

export type JsonValue = string | number | boolean | null | JsonObject | JsonValue[]

export interface JsonObject {
    [key: string]: JsonValue
}

export interface JsonRpcError {
    code: number
    message: string
    data?: JsonValue
}

export class JsonRpcResponseError extends Error {
    constructor(
        message: string,
        readonly code: number
    ) {
        super(message)
    }
}

export class JsonRpcProtocolMismatchError extends Error {
    constructor(message: string) {
        super(message)
        this.name = 'JsonRpcProtocolMismatchError'
    }
}

type JsonRpcId = number | string

export interface JsonRpcNotification {
    source: string
    method: string
    params?: JsonValue
    epoch?: string
    seq?: bigint
}

/**
 * A request a child subprocess sent to us (it carries both `id` and `method`).
 * The owner answers via `respond` / `respondError` exactly once. No broker-owned
 * worker currently sends inbound requests; `handleChildRequest` replies
 * method-not-found.
 */
export interface JsonRpcInboundRequest {
    source: string
    method: string
    params?: JsonValue
    id: JsonRpcId
}

interface PendingCall {
    resolve: (value: JsonValue | undefined) => void
    reject: (error: Error) => void
    timeout?: ReturnType<typeof setTimeout>
}

interface JsonRpcSubprocessEvents {
    notification: [JsonRpcNotification]
    request: [JsonRpcInboundRequest]
    log: [{ source: string; stream: 'stdout' | 'stderr'; text: string }]
    exit: [{ source: string; code: number | null }]
    unresponsive: [{ source: string }]
    'protocol-mismatch': [{ source: string; reason: string }]
}

const log = createStructuredLogger('service-bridge')

export class JsonRpcSubprocess extends EventEmitter<JsonRpcSubprocessEvents> {
    readonly name: string
    readonly binaryPath: string

    private child: ChildProcessWithoutNullStreams | null = null
    private pending = new Map<number, PendingCall>()
    private nextId = 0
    private writeTail: Promise<void> = Promise.resolve()

    /**
     * Protocol v2 epoch stamped on every notification this subprocess sends,
     * and this subprocess's notification counter. seq is per-subprocess (one
     * JsonRpcSubprocess per child, restarted with the child), so a reconnect
     * starts a fresh run — the same per-connection contract the Go codec keeps.
     *
     * Initialized to a local UUID so any notification sent before the peer's
     * first inbound frame still carries a non-empty epoch (satisfying the Go
     * codec's presence check). As soon as an inbound notification frame
     * carries a non-empty epoch, handleStdout adopts that peer epoch so both
     * ends agree on the broker's identity.
     */
    private epoch: string = randomUUID()
    private notificationSeq = 0n
    private protocolMismatch = false
    private silenceTimer: ReturnType<typeof setTimeout> | null = null
    private silenceGeneration = 0
    private silenceEnabled = false
    private silenceExpired = false

    constructor(name: string, binaryPath: string) {
        super()
        this.name = name
        this.binaryPath = binaryPath
    }

    get running(): boolean {
        return this.child !== null
    }

    start(args: string[] = []): void {
        if (this.child) return

        const child = spawn(this.binaryPath, args, {
            stdio: ['pipe', 'pipe', 'pipe'],
            detached: false
        })
        this.child = child
        this.epoch = randomUUID()
        this.notificationSeq = 0n
        this.protocolMismatch = false
        this.writeTail = Promise.resolve()
        this.silenceExpired = false
        this.silenceEnabled = true
        this.armSilenceDeadline(child)

        readline.createInterface({ input: child.stdout }).on('line', line => {
            if (this.child === child && !this.silenceExpired) this.handleStdout(line)
        })

        // Emitting is all this owes the line. The owner listening on 'log'
        // records it — at the severity the line reports — and writing here as
        // well is what put every service message in the log twice.
        readline.createInterface({ input: child.stderr }).on('line', line => {
            if (!line) return
            this.emit('log', {
                source: this.name,
                stream: 'stderr',
                text: redactSensitiveLogText(line)
            })
        })

        child.on('exit', code => {
            if (this.child !== child) return
            this.cancelSilenceDeadline()
            this.child = null
            this.failPending(new Error(`${this.name} exited before replying`))
            this.emit('exit', { source: this.name, code })
        })

        child.on('error', err => {
            if (this.child !== child) return
            this.cancelSilenceDeadline()
            this.child = null
            this.failPending(err)
            log.error({
                sublevel: this.name,
                message: `Failed to start ${this.name}: ${getErrorString(err)}`
            })
            // A spawn failure never fires 'exit', so emit it here too: callers
            // (e.g. the supervisor's broker-crash handler) treat a failure to
            // launch the same as an unexpected exit.
            this.emit('exit', { source: this.name, code: null })
        })
    }

    async call(
        method: string,
        params?: JsonValue,
        timeoutMs: number | null = 10_000
    ): Promise<JsonValue | undefined> {
        const child = this.child
        if (!child) throw new Error(`${this.name} is not running`)

        const id = ++this.nextId
        const message = { jsonrpc: '2.0', id, method, params }

        return new Promise((resolve, reject) => {
            const timeout =
                timeoutMs === null
                    ? undefined
                    : setTimeout(() => {
                          this.pending.delete(id)
                          reject(new Error(`${this.name} ${method} timed out`))
                      }, timeoutMs)
            this.pending.set(id, { resolve, reject, timeout })
            this.write(JSON.stringify(message)).catch(err => {
                if (timeout) clearTimeout(timeout)
                this.pending.delete(id)
                reject(err)
            })
        })
    }

    /**
     * Fire a request after its stdin write without waiting for the response.
     * Use {@link sendWithResponse} only when a caller must observe admission.
     * The backend runs it in its own goroutine and reports through notifications
     * (`engine:install-progress`, `engine:state-changed`, `errors:report`) per
     * the reactive command model — so there is no result to await and, crucially,
     * no short request timeout to fabricate a failure on long-running operations.
     */
    send(method: string, params?: JsonValue): Promise<void> {
        const id = ++this.nextId
        return this.write(JSON.stringify({ jsonrpc: '2.0', id, method, params }))
    }

    /** Observe a long-running request's eventual response without imposing a timeout. */
    sendWithResponse(method: string, params?: JsonValue): Promise<void> {
        return this.call(method, params, null).then(() => undefined)
    }

    notify(method: string, params?: JsonValue): Promise<void> {
        // Stamped here rather than in write(), because only a notification
        // carries the envelope: a request or response with epoch/seq on it is
        // rejected by the same v2 rule that requires them on a notification.
        //
        // The counter is bumped in the same synchronous block that enqueues the
        // write, so seq order and wire order cannot diverge: write() appends to
        // writeTail without awaiting, and nothing runs between the increment and
        // that append. Assigning the seq somewhere earlier (or awaiting in
        // between) would violate the sender's monotonic wire guarantee.
        // Receivers do not resync on gaps or regressions.
        const seq = ++this.notificationSeq
        const encodedSeq = encodeSeq(seq)
        const payload =
            params !== undefined
                ? `{"jsonrpc":"2.0","method":${JSON.stringify(method)},"params":${JSON.stringify(params)},"epoch":${JSON.stringify(this.epoch)},"seq":${encodedSeq}}`
                : `{"jsonrpc":"2.0","method":${JSON.stringify(method)},"epoch":${JSON.stringify(this.epoch)},"seq":${encodedSeq}}`
        return this.write(payload)
    }

    /** Reply to a child-initiated request (see {@link JsonRpcInboundRequest}). */
    respond(id: JsonRpcId, result: JsonValue): Promise<void> {
        return this.write(JSON.stringify({ jsonrpc: '2.0', id, result }))
    }

    respondError(id: JsonRpcId, code: number, message: string): Promise<void> {
        return this.write(JSON.stringify({ jsonrpc: '2.0', id, error: { code, message } }))
    }

    /**
     * Gracefully stop the subprocess: send `shutdown`, close stdin, then wait up
     * to `graceMs` for a clean exit before `SIGKILL`. `graceMs` exists because a
     * worker may need to tear down its own children on exit — notably
     * `nvpair-engine-manager`, which runs `StopAll()` to stop any engines it
     * launched (e.g. Ollama) so they aren't orphaned. SIGKILLing it mid-StopAll
     * would leave those engine processes running, so it gets a longer grace.
     *
     * The wait is reported because the grace is the only thing standing between a
     * blocked main process and a quit that looks like a backend hang: a starved
     * timer fires as soon as the loop resumes, so an instant shutdown can present
     * as the full grace elapsing. Logging the measured wait, and whether the child
     * was actually signalled, is what tells those two apart in a field log.
     */
    async stop(graceMs = 5_000): Promise<void> {
        this.cancelSilenceDeadline()
        const child = this.child
        if (!child) return

        try {
            await this.call('shutdown', null, 2_000)
        } catch {
            /* some helpers only shut down when stdin closes */
        }

        const startedAt = Date.now()
        child.stdin.end()
        const exited = await this.waitForExit(child, graceMs)
        const waitedMs = Date.now() - startedAt

        if (!exited) {
            child.kill('SIGKILL')
            log.warn({
                sublevel: this.name,
                message: `${this.name} did not exit within its ${graceMs}ms shutdown grace (waited ${waitedMs}ms); sent SIGKILL`
            })
        } else if (waitedMs > graceMs) {
            log.warn({
                sublevel: this.name,
                message: `${this.name} exited cleanly but the wait took ${waitedMs}ms, past its ${graceMs}ms grace — the main process was blocked, not the child`
            })
        } else {
            log.info({
                sublevel: this.name,
                message: `${this.name} exited in ${waitedMs}ms`
            })
        }
        this.child = null
    }

    /**
     * Resolve true if the child exited within `graceMs`, false if it is still
     * running and should be signalled.
     *
     * The `setImmediate` is the point. When the main process stalls, this timer
     * runs late with the child's `exit` already queued behind it, and killing
     * from the timer callback blames a process that has already stopped. Yielding
     * once lands in the check phase after the poll phase that delivers that exit,
     * so a pending exit is always seen before the escalation.
     */
    private waitForExit(child: ChildProcessWithoutNullStreams, graceMs: number): Promise<boolean> {
        // The child may already be gone: `exit` fires during the shutdown call
        // above, or while an earlier process in the teardown loop is being joined,
        // and a listener attached after that never fires again. Without this the
        // wait runs the entire grace and then reports a child that left early as a
        // blocked main process — pointing the new diagnostic at the wrong side.
        if (child.exitCode !== null || child.signalCode !== null) return Promise.resolve(true)

        return new Promise<boolean>(resolve => {
            let settled = false
            const finish = (exited: boolean): void => {
                if (settled) return
                settled = true
                clearTimeout(timer)
                child.removeListener('exit', onExit)
                resolve(exited)
            }
            const onExit = (): void => finish(true)
            const timer = setTimeout(() => {
                setImmediate(() => {
                    finish(child.exitCode !== null || child.signalCode !== null)
                })
            }, graceMs)
            child.once('exit', onExit)
        })
    }

    private async write(payload: string): Promise<void> {
        if (this.silenceExpired)
            throw new Error(`${this.name} connection lost: inbound silence timeout`)
        if (this.protocolMismatch)
            throw new JsonRpcProtocolMismatchError(
                `${this.name} connection closed due to protocol mismatch`
            )
        const child = this.child
        if (!child) throw new Error(`${this.name} is not running`)

        this.writeTail = this.writeTail.then(
            () =>
                new Promise<void>((resolve, reject) => {
                    if (this.child !== child || this.silenceExpired || this.protocolMismatch) {
                        reject(
                            new Error(
                                this.protocolMismatch
                                    ? `${this.name} connection closed due to protocol mismatch`
                                    : `${this.name} connection is no longer active`
                            )
                        )
                        return
                    }
                    child.stdin.write(`${payload}\n`, err => {
                        if (err) {
                            reject(err)
                            return
                        }
                        resolve()
                    })
                })
        )
        return this.writeTail
    }

    private handleStdout(line: string): void {
        if (!line) return
        // Redact only what is logged; `line` stays intact for the parse below so
        // the pairing flow still receives the real PIN it has to display.
        this.emit('log', {
            source: this.name,
            stream: 'stdout',
            text: redactSensitiveLogText(line)
        })

        if (this.protocolMismatch) return

        const frame = decodeRelayFrame(line)
        if (frame.kind === 'decode-error') {
            log.warn({
                sublevel: this.name,
                message: `${this.name} relay frame decode error: ${redactSensitiveLogText(frame.reason)}`
            })
            return
        }

        if (frame.kind === 'mismatch') {
            this.protocolMismatch = true
            this.cancelSilenceDeadline()
            this.failPending(new JsonRpcProtocolMismatchError(frame.reason))
            this.emit('protocol-mismatch', {
                source: this.name,
                reason: frame.reason
            })
            return
        }

        if (this.child) this.armSilenceDeadline(this.child)

        const message = frame.message
        if (message.id !== undefined && message.id !== null && message.method) {
            this.emit('request', {
                source: this.name,
                method: message.method,
                params: message.params,
                id: message.id
            })
            return
        }

        if (message.id !== undefined && message.id !== null) {
            this.deliverResponse(message)
            return
        }

        if (message.method) {
            const inboundEpoch =
                typeof message.epoch === 'string' && message.epoch.length > 0
                    ? message.epoch
                    : undefined
            if (inboundEpoch) {
                this.epoch = inboundEpoch
            }
            const inboundSeq = typeof message.seq === 'bigint' ? message.seq : undefined
            this.emit('notification', {
                source: this.name,
                method: message.method,
                params: message.params,
                ...(inboundEpoch !== undefined ? { epoch: inboundEpoch } : {}),
                ...(inboundSeq !== undefined ? { seq: inboundSeq } : {})
            })
        }
    }

    private deliverResponse(message: JsonRpcMessage): void {
        if (typeof message.id !== 'number') return
        const pending = this.pending.get(message.id)
        if (!pending) return

        this.pending.delete(message.id)
        if (pending.timeout) clearTimeout(pending.timeout)

        if (message.error) {
            pending.reject(
                new JsonRpcResponseError(
                    `${message.error.code}: ${message.error.message}`,
                    message.error.code
                )
            )
            return
        }

        pending.resolve(message.result)
    }

    private cancelSilenceDeadline(): void {
        this.silenceEnabled = false
        this.silenceGeneration++
        if (this.silenceTimer) clearTimeout(this.silenceTimer)
        this.silenceTimer = null
    }

    private armSilenceDeadline(child: ChildProcessWithoutNullStreams): void {
        if (!this.silenceEnabled) return
        if (this.silenceTimer) clearTimeout(this.silenceTimer)
        const generation = ++this.silenceGeneration
        this.silenceTimer = setTimeout(() => {
            if (
                this.child !== child ||
                generation !== this.silenceGeneration ||
                !this.silenceEnabled
            )
                return
            this.cancelSilenceDeadline()
            this.silenceExpired = true
            this.failPending(new Error(`${this.name} connection lost: inbound silence timeout`))
            // The owner retains the child handle and performs graceful stop/reap.
            // This is distinct from EOF/exit and never sends a watchdog RPC.
            this.emit('unresponsive', { source: this.name })
        }, MODULAR_HEARTBEAT_TIMEOUT_MS)
        this.silenceTimer.unref()
    }

    private failPending(error: Error): void {
        for (const pending of Array.from(this.pending.values())) {
            if (pending.timeout) clearTimeout(pending.timeout)
            pending.reject(error)
        }
        this.pending.clear()
    }
}
