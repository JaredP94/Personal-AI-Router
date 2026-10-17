// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { mkdtemp, mkdir, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { afterEach, describe, expect, it } from 'vitest'
import { readLicenseText } from '@/../scripts/collect-go-licenses'
import { renderLicenseText } from '@/../scripts/render-license-text'

describe('notice license text fencing', () => {
    it('keeps the standard fence when upstream text contains no backticks', () => {
        expect(renderLicenseText('Copyright upstream\nMIT License')).toBe(
            '```text\nCopyright upstream\nMIT License\n```'
        )
    })

    it('encloses embedded fenced examples without closing the outer block', () => {
        expect(renderLicenseText('README\n```sh\nnpm install example\n```')).toBe(
            '````text\nREADME\n```sh\nnpm install example\n```\n````'
        )
    })

    it('uses a fence longer than even an inline run of backticks', () => {
        expect(renderLicenseText('Keep ````` verbatim\n\n')).toBe(
            '``````text\nKeep ````` verbatim\n\n\n``````'
        )
    })
})

describe('Go module root attribution collection', () => {
    const directories: string[] = []

    async function moduleDirectory(): Promise<string> {
        const directory = await mkdtemp(join(tmpdir(), 'pair-license-test-'))
        directories.push(directory)
        return directory
    }

    afterEach(async () => {
        await Promise.all(
            directories.splice(0).map(directory => rm(directory, { recursive: true }))
        )
    })

    it('preserves a single upstream file without adding a filename label', async () => {
        const directory = await moduleDirectory()
        await writeFile(join(directory, 'LICENSE'), '  Upstream copyright\r\n\r\nMIT terms\n')
        expect(await readLicenseText(directory)).toBe('  Upstream copyright\r\n\r\nMIT terms\n')
    })

    it('includes every matching file in stable filename order with labels', async () => {
        const directory = await moduleDirectory()
        await writeFile(join(directory, 'NOTICE.txt'), 'Notice text\n')
        await writeFile(join(directory, 'LICENSE-MIT'), 'MIT terms\n')
        await writeFile(join(directory, 'COPYRIGHT'), 'Copyright holders\n')
        await writeFile(join(directory, 'COPYING.md'), 'Copying terms\n')
        await writeFile(join(directory, 'LICENSE'), 'Primary terms\n')
        await writeFile(join(directory, 'README.md'), 'Not a license file')
        await writeFile(join(directory, 'package.json'), '{"license":"MIT"}')
        await mkdir(join(directory, 'NOTICE-directory'))

        expect(await readLicenseText(directory)).toBe(
            '--- COPYING.md ---\nCopying terms\n\n\n' +
                '--- COPYRIGHT ---\nCopyright holders\n\n\n' +
                '--- LICENSE ---\nPrimary terms\n\n\n' +
                '--- LICENSE-MIT ---\nMIT terms\n\n\n' +
                '--- NOTICE.txt ---\nNotice text\n'
        )
    })

    it('leaves missing authoritative text unresolved rather than using a README', async () => {
        const directory = await moduleDirectory()
        await writeFile(join(directory, 'README.md'), '# License\nMIT')
        await writeFile(join(directory, 'LICENSE'), ' \n')
        expect(await readLicenseText(directory)).toBe('')
    })
})
