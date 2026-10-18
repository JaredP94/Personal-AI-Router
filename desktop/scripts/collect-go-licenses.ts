// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Collects license information for the Go modules linked into the shipped
 * service binaries.
 *
 * The desktop installer bundles the Go service components listed in
 * `services/versions.json` as `extraResources`, so their dependencies need
 * attribution in the canonical repository-root notice shipped with the app.
 *
 * Module sets are build-tag dependent — `github.com/Microsoft/go-winio` only
 * appears for `GOOS=windows` — so every release target is enumerated and the
 * results are unioned. Each service is its own Go module, so two binaries can
 * legitimately link different versions of the same dependency; those are kept as
 * separate entries.
 *
 * Attribution text is read from the module cache directory reported by
 * `go list -mod=readonly`. Go may use the network to resolve uncached dependencies;
 * readonly mode prevents changes to module manifests during notice generation.
 * In `--check` mode, GOPROXY=off and GOFLAGS=-mod=readonly ensure no network
 * downloads occur; an unpopulated cache fails with a message to run make deps-go.
 */
import { execFile } from 'node:child_process'
import { readdir, readFile } from 'node:fs/promises'
import { join } from 'node:path'
import { promisify } from 'node:util'
import { detectLicenseType } from '@/shared/utils/detect-license'

const execFileAsync = promisify(execFile)

export interface LicenseEntry {
    name: string
    version: string
    license: string
    author: string | null
    email: string | null
    repository: string | null
    homepage: string | null
    licenseText: string
}

/** Release targets the service build scripts support. */
const GO_TARGETS: ReadonlyArray<{ goos: string; goarch: string }> = [
    { goos: 'windows', goarch: 'amd64' },
    { goos: 'windows', goarch: 'arm64' },
    { goos: 'linux', goarch: 'amd64' },
    { goos: 'linux', goarch: 'arm64' },
    { goos: 'darwin', goarch: 'amd64' },
    { goos: 'darwin', goarch: 'arm64' }
]

/**
 * First-party modules in this repository. They are covered by the project's own
 * LICENSE and must not be listed as third-party components.
 */
const FIRST_PARTY_MODULES = new Set(['nvpair-shared', 'eapnoob'])

const LICENSE_FILE_PATTERN = /^(LICEN[SC]E|COPYING|NOTICE|COPYRIGHT)([.-][\w.-]+)?$/i

interface GoModule {
    path: string
    version: string
    dir: string
}

function isThirdParty(module: GoModule): boolean {
    if (FIRST_PARTY_MODULES.has(module.path)) return false
    // A main or replaced local module has no resolved upstream version.
    if (!module.version) return false
    if (module.version.startsWith('v0.0.0-00010101000000')) return false
    return module.dir.length > 0
}

async function listModulesForTarget(
    componentDir: string,
    goos: string,
    goarch: string,
    check = false
): Promise<GoModule[]> {
    const template = '{{if .Module}}{{.Module.Path}}\t{{.Module.Version}}\t{{.Module.Dir}}{{end}}'
    let stdout: string
    try {
        const result = await execFileAsync(
            'go',
            ['list', '-mod=readonly', '-deps', '-f', template, '.'],
            {
                cwd: componentDir,
                env: {
                    ...process.env,
                    GOOS: goos,
                    GOARCH: goarch,
                    CGO_ENABLED: '0',
                    ...(check ? { GOPROXY: 'off', GOFLAGS: '-mod=readonly' } : {})
                },
                maxBuffer: 32 * 1024 * 1024
            }
        )
        stdout = result.stdout
    } catch (err) {
        // A component that cannot resolve for one target must not silently drop
        // its dependencies from the notice file.
        const msg = String(err instanceof Error ? err.message : err)
        if (check) {
            throw new Error(
                `go list failed for ${componentDir} (${goos}/${goarch}): ${msg}\nModule cache may be missing; run 'make deps-go' first.`
            )
        }
        throw new Error(`go list failed for ${componentDir} (${goos}/${goarch}): ${msg}`)
    }

    const modules: GoModule[] = []
    for (const line of stdout.split('\n')) {
        if (!line.trim()) continue
        const [path, version, dir] = line.split('\t')
        modules.push({ path, version: version ?? '', dir: dir ?? '' })
    }
    return modules
}

export async function readLicenseText(moduleDir: string): Promise<string> {
    let names: string[]
    try {
        names = await readdir(moduleDir)
    } catch {
        return ''
    }

    // Include companion attribution files as well as license grants. Use code-point
    // filename order so output does not depend on filesystem order or locale.
    const candidates = names.filter(name => LICENSE_FILE_PATTERN.test(name)).sort()
    const texts: Array<{ name: string; text: string }> = []
    for (const name of candidates) {
        try {
            const text = await readFile(join(moduleDir, name), 'utf8')
            if (text.trim()) texts.push({ name, text })
        } catch {
            /* Skip directories or unreadable candidates; keep other attribution files. */
        }
    }
    return texts
        .map(({ name, text }) => (texts.length > 1 ? `--- ${name} ---\n${text}` : text))
        .join('\n\n')
}

function repositoryUrl(modulePath: string): string {
    return `https://${modulePath}`
}

export async function collectGoModuleEntries(
    moduleDirs: readonly string[],
    options?: { check?: boolean }
): Promise<LicenseEntry[]> {
    const byKey = new Map<string, GoModule>()
    const check = options?.check ?? false

    for (const moduleDir of moduleDirs) {
        for (const { goos, goarch } of GO_TARGETS) {
            for (const module of await listModulesForTarget(moduleDir, goos, goarch, check)) {
                if (!isThirdParty(module)) continue
                byKey.set(`${module.path}@${module.version}`, module)
            }
        }
    }

    const entries: LicenseEntry[] = []
    for (const module of byKey.values()) {
        const licenseText = await readLicenseText(module.dir)
        entries.push({
            name: module.path,
            version: module.version,
            // yaml.v3 incorporates MIT-licensed libyaml code alongside Apache terms.
            license: !licenseText
                ? 'Unknown'
                : module.path === 'gopkg.in/yaml.v3' && module.version === 'v3.0.1'
                  ? 'MIT AND Apache-2.0'
                  : detectLicenseType(licenseText),
            author: null,
            email: null,
            repository: repositoryUrl(module.path),
            homepage: null,
            licenseText: licenseText || '(no license file found in module)'
        })
    }

    return entries.sort((a, b) =>
        a.name === b.name ? a.version.localeCompare(b.version) : a.name.localeCompare(b.name)
    )
}
