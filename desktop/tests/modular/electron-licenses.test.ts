// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { afterEach, describe, expect, it } from 'vitest'
import { assertPackedElectronLicenses } from '@/../scripts/build/electron-licenses'

const directories: string[] = []

function packedApp(licenseDirectory: string): { appOutDir: string; licensesDir: string } {
    const appOutDir = mkdtempSync(join(tmpdir(), 'pair-electron-licenses-'))
    directories.push(appOutDir)
    const licensesDir = join(appOutDir, licenseDirectory)
    mkdirSync(licensesDir, { recursive: true })
    writeFileSync(join(licensesDir, 'LICENSES.chromium.html'), '<html>Chromium notices</html>')
    writeFileSync(join(licensesDir, 'LICENSE.electron.txt'), 'Electron license')
    return { appOutDir, licensesDir }
}

afterEach(() => {
    for (const directory of directories.splice(0)) {
        rmSync(directory, { recursive: true, force: true })
    }
})

for (const { platform, licenseDirectory } of [
    { platform: 'darwin', licenseDirectory: 'PAIR.app/Contents/Resources' },
    { platform: 'win32', licenseDirectory: '.' },
    { platform: 'linux', licenseDirectory: '.' }
]) {
    describe(`${platform} packed Electron licenses`, () => {
        it('accepts both license files in the platform-specific location', () => {
            const { appOutDir } = packedApp(licenseDirectory)
            expect(() => assertPackedElectronLicenses(appOutDir, platform, 'PAIR')).not.toThrow()
        })

        for (const fileName of ['LICENSES.chromium.html', 'LICENSE.electron.txt']) {
            it(`rejects a missing ${fileName}, even if it exists elsewhere in the app`, () => {
                const { appOutDir, licensesDir } = packedApp(licenseDirectory)
                rmSync(join(licensesDir, fileName))
                const wrongDirectory = join(appOutDir, 'wrong-location')
                mkdirSync(wrongDirectory)
                writeFileSync(join(wrongDirectory, fileName), 'Misplaced license')

                expect(() => assertPackedElectronLicenses(appOutDir, platform, 'PAIR')).toThrow(
                    fileName
                )
            })

            it(`rejects a directory named ${fileName}`, () => {
                const { appOutDir, licensesDir } = packedApp(licenseDirectory)
                rmSync(join(licensesDir, fileName))
                mkdirSync(join(licensesDir, fileName))

                expect(() => assertPackedElectronLicenses(appOutDir, platform, 'PAIR')).toThrow(
                    fileName
                )
            })
        }
    })
}
