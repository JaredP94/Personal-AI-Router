// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { execFileSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { afterEach, describe, expect, it } from 'vitest'

const desktop = process.cwd()
const directories: string[] = []

function generate(source: string): string {
    const root = mkdtempSync(path.join(tmpdir(), 'pair-contract-test-'))
    directories.push(root)
    const client = path.join(root, 'services/nvpair-tui')
    const cwd = path.join(root, 'desktop')
    mkdirSync(client, { recursive: true })
    mkdirSync(path.join(cwd, 'docs'), { recursive: true })
    mkdirSync(path.join(cwd, 'native/PrivilegedHelper'), { recursive: true })
    writeFileSync(path.join(client, 'client.go'), source)
    // Test-only methods must not become part of the published surface.
    writeFileSync(path.join(client, 'client_test.go'), 'call(client, "test:only", nil)')
    writeFileSync(
        path.join(root, 'services/versions.json'),
        JSON.stringify({ components: { 'nvpair-tui': '1.0.0' } })
    )
    writeFileSync(path.join(cwd, 'docs/service-contract-exceptions.json'), '{}')
    writeFileSync(
        path.join(cwd, 'native/PrivilegedHelper/main.swift'),
        'static let networkedBinaries = []'
    )
    execFileSync(
        path.join(desktop, 'node_modules/.bin/tsx'),
        [
            '--tsconfig',
            path.join(desktop, 'tsconfig.node.json'),
            path.join(desktop, 'scripts/verify-service-contracts.ts'),
            '--write'
        ],
        { cwd, encoding: 'utf8' }
    )
    return readFileSync(path.join(cwd, 'docs/services-api.md'), 'utf8')
}

afterEach(() => {
    for (const directory of directories.splice(0))
        rmSync(directory, { recursive: true, force: true })
})

describe('TUI service contract generation', () => {
    it('classifies literal and variable requests through both helpers and direct calls as outgoing', () => {
        const doc = generate(`package ui
func install() {
    call(client, "engine:models", nil, decode)
    call(client, "ping", nil, decode)
    callWithTimeout(client, "engine:set-port", nil, deadline, decode)
    client.Call(ctx, "engine:uninstall", nil)
    client.Call(ctx, "shutdown", nil)
    snapshotQuery(client, "workers:get-status", convert, failed)
}
func deleteModel() {
    method := "engine:action"
    if peer { method = "engine:remote-delete-model" }
    callWithTimeout(client, method, params, deadline, decode)
}
func installed() {
    method := "engine:get-installed"
    if peer { method = "engine:remote-get-installed" }
    call(client, method, nil, decode)
}
// A wrapper's parameter must not resolve to a different function's assignment.
func (c *Client) Notify(method string, params any) { c.codec.Notify(method, params) }
`)
        for (const method of [
            'engine:models',
            'ping',
            'shutdown',
            'engine:set-port',
            'engine:uninstall',
            'workers:get-status',
            'engine:action',
            'engine:remote-delete-model',
            'engine:get-installed',
            'engine:remote-get-installed'
        ]) {
            expect(doc).toContain(`| \`${method}\` | request (TUI sends) |`)
            expect(doc).not.toContain(`| \`${method}\` | notification`)
        }
        expect(doc).not.toContain('test:only')
    })

    it('distinguishes consumed pushes from nested stage cases and ordinary request arguments', () => {
        const doc = generate(`package ui
func update() {
    switch msg.Msg.Method {
    case "engine:models-changed", "engine:state-changed":
        switch stage {
        case "error":
        }
    }
    if msg.Msg.Method == "workers:update" {}
    call(client, "workers:restart", nil, decode)
}
`)
        for (const method of ['engine:models-changed', 'engine:state-changed', 'workers:update']) {
            expect(doc).toContain(`| \`${method}\` | notification (TUI consumes) |`)
            expect(doc).not.toContain(`| \`${method}\` | request`)
        }
        expect(doc).toContain('| `workers:restart` | request (TUI sends) |')
        expect(doc).not.toContain('| `error` |')
    })

    it('reports computed request methods as unresolved outgoing sites rather than notifications', () => {
        const doc = generate(`package ui
func remote() {
    method, what = "engine:start", "start"
    if peer { method = "engine:remote-" + what }
    callWithTimeout(client, method, params, deadline, decode)
}
func proxy() { call(client, e.prefix+":get-status", nil, decode) }
func logs() { call(client, applog.SetLevelMethod, params, decode) }
`)
        expect(doc).toContain('| `engine:start` | request (TUI sends) |')
        expect(doc).toContain('Dynamic / unresolved request sites')
        expect(doc).toContain('engine:remote-*')
        expect(doc).toContain('e.prefix+":get-status"')
        expect(doc).toContain('applog.SetLevelMethod')
        expect(doc).not.toContain('| `engine:remote-` |')
    })
})
