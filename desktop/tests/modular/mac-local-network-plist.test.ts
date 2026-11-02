// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { readFileSync } from 'node:fs'
import ts from 'typescript'
import { expect, it } from 'vitest'

it('declares local-network permission and the discovery Bonjour service in mac.extendInfo', () => {
    const path = new URL('../../electron-builder.config.ts', import.meta.url)
    const source = ts.createSourceFile(
        'electron-builder.config.ts',
        readFileSync(path, 'utf8'),
        ts.ScriptTarget.Latest,
        true
    )
    const values = new Map<string, string>()
    function visit(node: ts.Node): void {
        if (
            ts.isPropertyAssignment(node) &&
            node.name.getText(source) === 'mac' &&
            ts.isObjectLiteralExpression(node.initializer)
        ) {
            for (const property of node.initializer.properties) {
                if (
                    !ts.isPropertyAssignment(property) ||
                    property.name.getText(source) !== 'extendInfo' ||
                    !ts.isObjectLiteralExpression(property.initializer)
                )
                    continue
                for (const entry of property.initializer.properties) {
                    if (ts.isPropertyAssignment(entry))
                        values.set(entry.name.getText(source), entry.initializer.getText(source))
                }
            }
        }
        ts.forEachChild(node, visit)
    }
    visit(source)
    expect(values.get('NSLocalNetworkUsageDescription')).toBe(
        "'PAIR uses the local network to discover and connect to your other PAIR nodes.'"
    )
    expect(values.get('NSBonjourServices')).toBe("['_nvpair-node._tcp']")
})
