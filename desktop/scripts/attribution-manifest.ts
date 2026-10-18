// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { createHash } from 'node:crypto'
import { existsSync, readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import type { LicenseEntry } from './collect-go-licenses'

export interface ManifestEntry {
    name: string
    version: string
    kind: 'npm' | 'go' | 'asset'
    license: string
    status: 'resolved' | 'blocked'
    sourceUrl: string
    sourceRef: string
    textFile: string | null
    textSha256: string | null
    notes: string
}

export interface AttributionManifest {
    entries: ManifestEntry[]
}

interface RawEntry {
    name?: string
    version?: string
    kind?: string
    license?: string
    status?: string
    sourceUrl?: string
    sourceRef?: string
    textFile?: string | null
    textSha256?: string | null
    notes?: string
}

interface RawManifest {
    entries?: RawEntry[]
}

export function loadAttributionManifest(
    manifestPath: string,
    textsDir?: string
): AttributionManifest {
    const raw = readFileSync(manifestPath, 'utf8')
    const data: RawManifest = JSON.parse(raw)
    if (typeof data !== 'object' || data === null || !Array.isArray(data.entries)) {
        throw new Error(`Invalid manifest at ${manifestPath}: expected 'entries' array`)
    }

    const seenKeys = new Set<string>()
    const entries: ManifestEntry[] = []
    const resolvedTextsDir = textsDir ?? resolve(dirname(manifestPath), 'texts')

    for (const entry of data.entries) {
        const name = entry.name
        const version = entry.version
        if (typeof name !== 'string' || !name.trim()) {
            throw new Error('Attribution manifest entry missing name')
        }
        if (typeof version !== 'string') {
            throw new Error(`Attribution manifest entry missing version for ${name}`)
        }
        const key = `${name}@${version}`
        if (seenKeys.has(key)) {
            throw new Error(`Duplicate attribution manifest entry: ${key}`)
        }
        seenKeys.add(key)

        const kind = entry.kind
        if (kind !== 'npm' && kind !== 'go' && kind !== 'asset') {
            throw new Error(`Invalid kind '${kind ?? ''}' for ${key}`)
        }

        const license = entry.license
        if (typeof license !== 'string' || !license.trim()) {
            throw new Error(`Missing license for ${key}`)
        }

        const status = entry.status
        if (status !== 'resolved' && status !== 'blocked') {
            throw new Error(`Invalid status '${status ?? ''}' for ${key}`)
        }

        const sourceUrl = typeof entry.sourceUrl === 'string' ? entry.sourceUrl : ''
        const sourceRef = typeof entry.sourceRef === 'string' ? entry.sourceRef : ''
        const notes = typeof entry.notes === 'string' ? entry.notes : ''

        let textFile: string | null = null
        if (typeof entry.textFile === 'string') {
            textFile = entry.textFile
        }

        let textSha256: string | null = null
        if (typeof entry.textSha256 === 'string') {
            textSha256 = entry.textSha256
        }

        if (status === 'resolved') {
            if (textFile === null || !textFile.trim()) {
                throw new Error(`Resolved attribution manifest entry missing textFile for ${key}`)
            }
            if (textSha256 === null || !textSha256.trim()) {
                throw new Error(`Resolved attribution manifest entry missing textSha256 for ${key}`)
            }
            const fullPath = resolve(resolvedTextsDir, textFile)
            if (!existsSync(fullPath)) {
                throw new Error(`Attribution text file for ${key} not found: ${fullPath}`)
            }
            const content = readFileSync(fullPath, 'utf8')
            const actualSha256 = createHash('sha256').update(content).digest('hex')
            if (actualSha256.toLowerCase() !== textSha256.toLowerCase()) {
                throw new Error(
                    `Attribution text SHA-256 mismatch for ${key}: expected ${textSha256}, got ${actualSha256}`
                )
            }
        }

        entries.push({
            name,
            version,
            kind,
            license,
            status,
            sourceUrl,
            sourceRef,
            textFile,
            textSha256,
            notes
        })
    }

    return { entries }
}

const README_FALLBACK_MARKER =
    '(README fallback; authoritative license text requires human review.)'
const NO_LICENSE_FILE_MARKER = '(no license file found'

export function applyAttribution(
    entries: LicenseEntry[],
    manifest: AttributionManifest,
    readText: (file: string) => string
): { entries: LicenseEntry[]; unresolved: string[] } {
    const manifestByKey = new Map<string, ManifestEntry>()
    for (const item of manifest.entries) {
        manifestByKey.set(`${item.name}@${item.version}`, item)
    }

    const unresolvedSet = new Set<string>()
    const resultEntries: LicenseEntry[] = []

    for (const entry of entries) {
        const key = `${entry.name}@${entry.version}`
        const manifestEntry = manifestByKey.get(key)
        if (manifestEntry !== undefined && manifestEntry.status === 'resolved') {
            const textFile = manifestEntry.textFile
            if (textFile === null) {
                throw new Error(`Resolved manifest entry ${key} has no textFile`)
            }
            resultEntries.push({
                ...entry,
                license: manifestEntry.license,
                licenseText: readText(textFile)
            })
        } else {
            resultEntries.push(entry)
            const text = entry.licenseText ?? ''
            const isFallback =
                text.includes(README_FALLBACK_MARKER) || text.includes(NO_LICENSE_FILE_MARKER)
            if (isFallback) {
                unresolvedSet.add(key)
            }
        }
    }

    for (const item of manifest.entries) {
        if (item.status === 'blocked') {
            unresolvedSet.add(`${item.name}@${item.version}`)
        }
    }

    for (const item of manifest.entries) {
        if (item.kind === 'asset') {
            const alreadyPresent = resultEntries.some(
                e => e.name === item.name && e.version === item.version
            )
            if (!alreadyPresent) {
                let assetLicenseText = ''
                if (item.textFile !== null) {
                    assetLicenseText = readText(item.textFile)
                } else {
                    assetLicenseText = '(no license file found)'
                }
                resultEntries.push({
                    name: item.name,
                    version: item.version,
                    license: item.license,
                    author: null,
                    email: null,
                    repository: item.sourceUrl ? item.sourceUrl : null,
                    homepage: null,
                    licenseText: assetLicenseText
                })
            }
        }
    }

    resultEntries.sort((a, b) =>
        a.name === b.name ? a.version.localeCompare(b.version) : a.name.localeCompare(b.name)
    )

    const unresolved = [...unresolvedSet].sort()
    return { entries: resultEntries, unresolved }
}
