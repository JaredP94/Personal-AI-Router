// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Generates the repository's single third-party notice, `THIRD_PARTY_NOTICES.md`
 * at the repository root, covering:
 *   - every runtime (production) npm dependency + all their transitives
 *   - the Electron runtime itself (declared as a devDependency, but shipped
 *     with the packaged app, so legally it has to appear in the report)
 *   - the Go modules linked into service components listed in
 *     `services/versions.json`, shipped by the installer as extraResources
 *
 * Dev-only tooling (vite, tsx, typescript, prettier, knip, etc.) is deliberately
 * excluded — it is not redistributed and does not need attribution.
 *
 * The repository-root notice is canonical; services/THIRD_PARTY_NOTICES.md
 * points to it. The desktop packaging configuration ships the canonical file.
 * Collected metadata and texts need human review; this is not a completeness
 * certification or an inventory of separately installed software.
 *
 * Run:
 *   npm run licenses            writes the notice (invoked on every build)
 *   npm run licenses:check      verifies the notice is up to date and all attributions resolved
 */
import { init, type InitOpts, type ModuleInfo, type ModuleInfos } from 'license-checker-rseidelsohn'
import { readFileSync } from 'node:fs'
import { readFile, writeFile } from 'node:fs/promises'
import { basename, dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import pkg from '../package.json' with { type: 'json' }
import { applyAttribution, loadAttributionManifest } from './attribution-manifest'
import { collectGoModuleEntries, type LicenseEntry } from './collect-go-licenses'
import { renderLicenseText } from './render-license-text'

const __dirname = dirname(fileURLToPath(import.meta.url))
const repoRoot = resolve(__dirname, '..')
const servicesDir = resolve(repoRoot, '..', 'services')

const NOTICE_PATH = resolve(repoRoot, '..', 'THIRD_PARTY_NOTICES.md')
const ELECTRON_DIR = join(repoRoot, 'node_modules/electron')
const SERVICE_VERSIONS_PATH = join(servicesDir, 'versions.json')
const MANIFEST_PATH = resolve(repoRoot, 'licenses', 'attribution-manifest.json')
const TEXTS_DIR = resolve(repoRoot, 'licenses', 'texts')

type Entry = LicenseEntry

function runChecker(opts: InitOpts): Promise<ModuleInfos> {
    return new Promise((resolvePromise, rejectPromise) => {
        init(opts, (err, modules) => {
            if (err) {
                rejectPromise(err)
                return
            }
            resolvePromise(modules)
        })
    })
}

function parseKey(key: string): { name: string; version: string } {
    const at = key.lastIndexOf('@')
    if (at <= 0) return { name: key, version: '' }
    return { name: key.slice(0, at), version: key.slice(at + 1) }
}

function normalizeLicense(licenses: string | string[] | undefined): string {
    if (licenses == null) return 'UNKNOWN'
    if (Array.isArray(licenses)) return licenses.join(' OR ')
    return licenses
}

function toEntry(key: string, info: ModuleInfo): Entry {
    const { name, version } = parseKey(key)
    const licenseText = (info.licenseText ?? '').trim()
    const readmeFallback = /^readme(?:[.-]|$)/i.test(basename(info.licenseFile ?? ''))
    return {
        name,
        version,
        license: normalizeLicense(info.licenses),
        author: info.publisher ?? null,
        email: info.email ?? null,
        repository: info.repository ?? null,
        homepage: info.url ?? null,
        licenseText: !licenseText
            ? '(no license file found in package)'
            : readmeFallback
              ? `(README fallback; authoritative license text requires human review.)\n\n${licenseText}`
              : licenseText
    }
}

async function buildElectronEntry(): Promise<Entry> {
    const rawPkg = await readFile(join(ELECTRON_DIR, 'package.json'), 'utf8')
    const electronPkg: {
        name: string
        version: string
        license?: string
        author?: string | { name?: string; email?: string; url?: string }
        homepage?: string
        repository?: string | { url?: string }
    } = JSON.parse(rawPkg)

    const licenseText = await readFile(join(ELECTRON_DIR, 'LICENSE'), 'utf8')

    const author =
        typeof electronPkg.author === 'string'
            ? electronPkg.author
            : (electronPkg.author?.name ?? null)
    const email =
        typeof electronPkg.author === 'string' ? null : (electronPkg.author?.email ?? null)
    const repository =
        typeof electronPkg.repository === 'string'
            ? electronPkg.repository
            : (electronPkg.repository?.url ?? null)

    return {
        name: electronPkg.name,
        version: electronPkg.version,
        license: electronPkg.license ?? 'UNKNOWN',
        author,
        email,
        repository,
        homepage: electronPkg.homepage ?? null,
        licenseText: licenseText.trim()
    }
}

function mergeEntries(scan: ModuleInfos, extras: Entry[]): Entry[] {
    const byKey = new Map<string, Entry>()
    for (const [key, info] of Object.entries(scan)) {
        const { name } = parseKey(key)
        if (name === pkg.name) continue
        byKey.set(key, toEntry(key, info))
    }
    for (const entry of extras) {
        byKey.set(`${entry.name}@${entry.version}`, entry)
    }
    return [...byKey.values()].sort((a, b) =>
        a.name === b.name ? a.version.localeCompare(b.version) : a.name.localeCompare(b.name)
    )
}

function renderMarkdown(entries: Entry[]): string {
    const parts: string[] = [
        '<!--',
        'SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.',
        'SPDX-License-Identifier: Apache-2.0',
        '-->',
        '',
        '# Third-Party Software Notices',
        '',
        '> **Generated file.** Run `cd desktop && npm run licenses` after changing Go',
        '> modules, npm dependencies, Electron, or license-generation logic.',
        '',
        'NVIDIA Personal AI Router is distributed under the Apache License 2.0 (see',
        '`LICENSE`). It incorporates the third-party open-source software listed below.',
        'Each component belongs to its respective copyright holders and is distributed',
        'under its own license. This file collects package/module-provided license and',
        'attribution texts with automatically derived license metadata. The npm scanner',
        'may fall back to a README rather than an authoritative license file; missing',
        'upstream text is marked where detected. Neither a README nor a license label',
        'alone establishes that all required notices are present. Human license review',
        'is required; this generated inventory is not a certification of completeness.',
        '',
        'Scope: production npm dependencies from the installed desktop dependency tree,',
        'the Electron runtime, and Go modules linked into the service components listed',
        'in `services/versions.json`, across Windows, Linux, and macOS targets.',
        'First-party modules (`nvpair-shared`, `eapnoob`) are excluded. This repository-root',
        'file is canonical; `services/THIRD_PARTY_NOTICES.md` is only a pointer.',
        '',
        'Electron Chromium dependency notices ship separately with the Electron runtime',
        'as `LICENSES.chromium.html`; they are not reproduced here. Optional inference',
        'engines (Ollama, LM Studio, oMLX) and the Phoenix container image are installed',
        'separately, not bundled in the desktop application, and are governed by their',
        'upstream terms. Vendored Kaizen CSS attribution is outside the npm dependency',
        'graph and requires separate review, including preservation in built CSS.',
        '',
        '## Components',
        '',
        '| Component | Version | License |',
        '| --- | --- | --- |'
    ]

    for (const e of entries) {
        parts.push(`| \`${e.name}\` | ${e.version} | ${e.license} |`)
    }

    parts.push('', '## License texts', '')

    for (const e of entries) {
        parts.push(`### ${e.name} ${e.version}`, '', `License: ${e.license}`, '')
        if (e.repository !== null) parts.push(`Repository: ${e.repository}`, '')
        parts.push(renderLicenseText(e.licenseText), '')
    }

    return `${parts.join('\n').trimEnd()}\n`
}

async function serviceComponentNames(): Promise<string[]> {
    const raw = await readFile(SERVICE_VERSIONS_PATH, 'utf8')
    const parsed: { components?: Record<string, string> } = JSON.parse(raw)
    const components = parsed.components
    if (components === undefined || Object.keys(components).length === 0) {
        throw new Error(`No components listed in ${SERVICE_VERSIONS_PATH}`)
    }
    return Object.keys(components)
}

async function main(): Promise<void> {
    const isCheck = process.argv.includes('--check')

    const prod = await runChecker({
        start: repoRoot,
        production: true,
        excludePrivatePackages: true,
        customFormat: { licenseText: '' }
    })

    const electronEntry = await buildElectronEntry()

    const componentNames = await serviceComponentNames()
    const serviceModuleDirs = componentNames.map(name => join(servicesDir, name))
    const scriptModuleDirs = [
        resolve(repoRoot, '..', 'scripts', 'collectlogs'),
        resolve(repoRoot, '..', 'scripts', 'inference-dispatcher')
    ]
    const moduleDirs = [...serviceModuleDirs, ...scriptModuleDirs]

    const goEntries = await collectGoModuleEntries(moduleDirs, { check: isCheck })
    const scannedEntries = mergeEntries(prod, [electronEntry, ...goEntries])

    const manifest = loadAttributionManifest(MANIFEST_PATH, TEXTS_DIR)
    const { entries, unresolved } = applyAttribution(scannedEntries, manifest, file =>
        readFileSync(resolve(TEXTS_DIR, file), 'utf8')
    )

    const rendered = renderMarkdown(entries)

    if (isCheck) {
        let noticeDiff = false
        try {
            const currentNotice = await readFile(NOTICE_PATH, 'utf8')
            if (currentNotice !== rendered) {
                noticeDiff = true
            }
        } catch {
            noticeDiff = true
        }

        if (noticeDiff) {
            console.error(
                `Error: ${NOTICE_PATH} is out of date. Run 'npm run licenses' to regenerate.`
            )
        }

        if (unresolved.length > 0) {
            console.error(`Error: Found ${unresolved.length} unresolved attribution(s):`)
            for (const key of unresolved) {
                console.error(`  - ${key}`)
            }
        }

        if (noticeDiff || unresolved.length > 0) {
            process.exit(1)
        }

        console.log(
            `OK — ${entries.length} dependencies (${goEntries.length} Go modules) attributed in ${NOTICE_PATH}`
        )
    } else {
        await writeFile(NOTICE_PATH, rendered)
        console.log(
            `Wrote ${entries.length} dependencies (${goEntries.length} Go modules) to ${NOTICE_PATH}`
        )
        if (unresolved.length > 0) {
            console.warn(`Warning: ${unresolved.length} unresolved attribution(s):`)
            for (const key of unresolved) {
                console.warn(`  warning: unresolved attribution for ${key}`)
            }
        }
    }
}

main().catch((err: unknown) => {
    console.error(err)
    process.exit(1)
})
