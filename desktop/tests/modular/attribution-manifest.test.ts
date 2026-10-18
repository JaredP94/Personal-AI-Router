// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { createHash } from 'node:crypto'
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { afterEach, describe, expect, it } from 'vitest'
import {
    applyAttribution,
    loadAttributionManifest,
    type AttributionManifest
} from '@/../scripts/attribution-manifest'
import type { LicenseEntry } from '@/../scripts/collect-go-licenses'

describe('attribution manifest', () => {
    const directories: string[] = []

    async function makeTempDir(): Promise<string> {
        const dir = await mkdtemp(join(tmpdir(), 'pair-manifest-test-'))
        directories.push(dir)
        return dir
    }

    afterEach(async () => {
        await Promise.all(directories.splice(0).map(dir => rm(dir, { recursive: true })))
    })

    it('resolved entry replaces fallback text', () => {
        const fixtureText = 'Authoritative license terms for pkg'
        const manifest: AttributionManifest = {
            entries: [
                {
                    name: 'pkg',
                    version: '1.0.0',
                    kind: 'npm',
                    license: 'MIT',
                    status: 'resolved',
                    sourceUrl: 'https://example.com/pkg',
                    sourceRef: 'v1.0.0',
                    textFile: 'pkg-1.0.0.txt',
                    textSha256: createHash('sha256').update(fixtureText).digest('hex'),
                    notes: 'Reviewed'
                }
            ]
        }

        const initialEntries: LicenseEntry[] = [
            {
                name: 'pkg',
                version: '1.0.0',
                license: 'UNKNOWN',
                author: null,
                email: null,
                repository: null,
                homepage: null,
                licenseText:
                    '(README fallback; authoritative license text requires human review.)\n\nReadme'
            }
        ]

        const { entries, unresolved } = applyAttribution(
            initialEntries,
            manifest,
            () => fixtureText
        )
        expect(entries).toHaveLength(1)
        expect(entries[0].license).toBe('MIT')
        expect(entries[0].licenseText).toBe(fixtureText)
        expect(unresolved).toEqual([])
    })

    it('fallback without manifest entry is unresolved', () => {
        const manifest: AttributionManifest = { entries: [] }
        const initialEntries: LicenseEntry[] = [
            {
                name: 'pkg',
                version: '1.0.0',
                license: 'UNKNOWN',
                author: null,
                email: null,
                repository: null,
                homepage: null,
                licenseText:
                    '(README fallback; authoritative license text requires human review.)\n\nReadme'
            }
        ]

        const { unresolved } = applyAttribution(initialEntries, manifest, () => '')
        expect(unresolved).toEqual(['pkg@1.0.0'])
    })

    it('blocked entry stays unresolved', () => {
        const manifest: AttributionManifest = {
            entries: [
                {
                    name: 'pkg',
                    version: '1.0.0',
                    kind: 'npm',
                    license: 'UNKNOWN',
                    status: 'blocked',
                    sourceUrl: 'https://example.com/pkg',
                    sourceRef: 'v1.0.0',
                    textFile: null,
                    textSha256: null,
                    notes: 'No upstream license found'
                }
            ]
        }
        const initialEntries: LicenseEntry[] = [
            {
                name: 'pkg',
                version: '1.0.0',
                license: 'UNKNOWN',
                author: null,
                email: null,
                repository: null,
                homepage: null,
                licenseText: '(no license file found in package)'
            }
        ]

        const { unresolved } = applyAttribution(initialEntries, manifest, () => '')
        expect(unresolved).toEqual(['pkg@1.0.0'])
    })

    it('asset entry is appended once', () => {
        const assetText = 'Custom asset license'
        const manifest: AttributionManifest = {
            entries: [
                {
                    name: 'kaizen-asset',
                    version: '1.0.0',
                    kind: 'asset',
                    license: 'Apache-2.0',
                    status: 'resolved',
                    sourceUrl: 'https://example.com/asset',
                    sourceRef: 'v1.0.0',
                    textFile: 'asset.txt',
                    textSha256: 'somehash',
                    notes: 'Asset'
                }
            ]
        }
        const initialEntries: LicenseEntry[] = []

        const { entries, unresolved } = applyAttribution(initialEntries, manifest, () => assetText)
        expect(entries).toHaveLength(1)
        expect(entries[0].name).toBe('kaizen-asset')
        expect(entries[0].version).toBe('1.0.0')
        expect(entries[0].license).toBe('Apache-2.0')
        expect(entries[0].licenseText).toBe(assetText)
        expect(entries[0].repository).toBe('https://example.com/asset')
        expect(unresolved).toEqual([])

        const { entries: secondRun } = applyAttribution(entries, manifest, () => assetText)
        expect(secondRun).toHaveLength(1)
    })

    it('hash mismatch is rejected', async () => {
        const dir = await makeTempDir()
        const textsDir = join(dir, 'texts')
        await mkdir(textsDir)
        const textFile = 'pkg-1.0.0.txt'
        await writeFile(join(textsDir, textFile), 'Actual text content')
        const manifestPath = join(dir, 'attribution-manifest.json')
        await writeFile(
            manifestPath,
            JSON.stringify({
                entries: [
                    {
                        name: 'pkg',
                        version: '1.0.0',
                        kind: 'npm',
                        license: 'MIT',
                        status: 'resolved',
                        sourceUrl: 'https://example.com',
                        sourceRef: 'v1.0.0',
                        textFile,
                        textSha256:
                            '0000000000000000000000000000000000000000000000000000000000000000',
                        notes: 'Mismatch test'
                    }
                ]
            })
        )

        expect(() => loadAttributionManifest(manifestPath, textsDir)).toThrow('pkg@1.0.0')
    })

    it('duplicate key is rejected', async () => {
        const dir = await makeTempDir()
        const manifestPath = join(dir, 'attribution-manifest.json')
        await writeFile(
            manifestPath,
            JSON.stringify({
                entries: [
                    {
                        name: 'pkg',
                        version: '1.0.0',
                        kind: 'npm',
                        license: 'MIT',
                        status: 'blocked',
                        sourceUrl: 'https://example.com',
                        sourceRef: 'v1.0.0',
                        textFile: null,
                        textSha256: null,
                        notes: 'First'
                    },
                    {
                        name: 'pkg',
                        version: '1.0.0',
                        kind: 'npm',
                        license: 'MIT',
                        status: 'blocked',
                        sourceUrl: 'https://example.com',
                        sourceRef: 'v1.0.0',
                        textFile: null,
                        textSha256: null,
                        notes: 'Duplicate'
                    }
                ]
            })
        )

        expect(() => loadAttributionManifest(manifestPath)).toThrow('pkg@1.0.0')
    })

    it('accepts an empty manifest', async () => {
        const dir = await makeTempDir()
        const manifestPath = join(dir, 'attribution-manifest.json')
        await writeFile(manifestPath, JSON.stringify({ entries: [] }))
        const manifest = loadAttributionManifest(manifestPath)
        expect(manifest.entries).toEqual([])
    })
})
