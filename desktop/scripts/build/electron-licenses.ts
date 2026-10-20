// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { statSync } from 'node:fs'
import { join } from 'node:path'

/** Fail packaging rather than silently shipping without Electron's license notices. */
export function assertPackedElectronLicenses(
    appOutDir: string,
    platform: string,
    productFilename: string
): void {
    // electron-builder retains these at the app root on Windows/Linux. On
    // macOS our extraResources copy puts them inside the .app bundle instead.
    const licensesDir =
        platform === 'darwin'
            ? join(appOutDir, `${productFilename}.app`, 'Contents', 'Resources')
            : appOutDir

    for (const fileName of ['LICENSES.chromium.html', 'LICENSE.electron.txt']) {
        const filePath = join(licensesDir, fileName)
        if (!statSync(filePath, { throwIfNoEntry: false })?.isFile()) {
            throw new Error(`Refusing to package without Electron license file: ${filePath}`)
        }
    }
}
