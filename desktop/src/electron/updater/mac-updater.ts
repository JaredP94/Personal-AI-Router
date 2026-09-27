// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { app } from 'electron'
import { spawn } from 'child_process'
import fs from 'fs'
import http from 'http'
import https from 'https'
import path from 'path'
import log from 'electron-log'
import getErrorString from '@/shared/utils/get-error-string'
import { destroyConnector } from '@/electron/connector'
import {
    clearPendingUpdate,
    writePendingUpdateVersion
} from '@/electron/updater/pending-update-store'

function getMacDmgFileName(version: string): string {
    const arch = process.arch === 'arm64' ? 'arm64' : 'x64'
    return `NVPAIR-Setup-${version}-${arch}.dmg`
}

function getMacUpdatesDir(): string {
    return path.join(app.getPath('userData'), 'updates')
}

export function getCachedMacDmgPath(version: string): string | null {
    const dmgPath = path.join(getMacUpdatesDir(), getMacDmgFileName(version))
    return fs.existsSync(dmgPath) ? dmgPath : null
}

export function cleanOldMacDmgs(currentVersion: string): void {
    try {
        const updatesDir = getMacUpdatesDir()
        if (!fs.existsSync(updatesDir)) return
        const entries = fs.readdirSync(updatesDir)
        for (const entry of entries) {
            if (entry.endsWith('.dmg') || entry.endsWith('.downloading')) {
                if (entry.includes(currentVersion)) {
                    fs.rmSync(path.join(updatesDir, entry), { force: true })
                }
            }
        }
    } catch {
        /* best-effort */
    }
}

function fetchBinaryFile(
    urlString: string,
    destPath: string,
    onProgress: (percent: number) => void,
    maxRedirects = 5
): Promise<void> {
    return new Promise((resolve, reject) => {
        if (maxRedirects < 0) {
            reject(new Error('Too many redirects while downloading update'))
            return
        }

        const parsedUrl = new URL(urlString)
        const client = parsedUrl.protocol === 'http:' ? http : https

        const req = client.get(
            urlString,
            {
                headers: {
                    'User-Agent': 'Personal-AI-Router-Updater',
                    Accept: 'application/octet-stream'
                }
            },
            res => {
                const code = res.statusCode ?? 0
                if (code >= 300 && code < 400 && res.headers.location) {
                    res.resume()
                    const nextUrl = new URL(res.headers.location, parsedUrl).toString()
                    fetchBinaryFile(nextUrl, destPath, onProgress, maxRedirects - 1)
                        .then(resolve)
                        .catch(reject)
                    return
                }

                if (code !== 200) {
                    res.resume()
                    reject(new Error(`Failed to download update: HTTP ${code}`))
                    return
                }

                const totalBytes = Number(res.headers['content-length'] ?? '0')
                let receivedBytes = 0
                const fileStream = fs.createWriteStream(destPath)

                res.on('data', (chunk: Buffer) => {
                    receivedBytes += chunk.length
                    if (totalBytes > 0) {
                        const pct = Math.min(100, Math.round((receivedBytes / totalBytes) * 100))
                        onProgress(pct)
                    }
                })

                res.pipe(fileStream)

                fileStream.on('finish', () => {
                    fileStream.close(() => resolve())
                })

                fileStream.on('error', err => {
                    fs.rmSync(destPath, { force: true })
                    reject(err)
                })

                res.on('error', err => {
                    fs.rmSync(destPath, { force: true })
                    reject(err)
                })
            }
        )

        req.on('error', err => {
            fs.rmSync(destPath, { force: true })
            reject(err)
        })
    })
}

export async function downloadMacUpdate(
    version: string,
    onProgress: (percent: number) => void
): Promise<string> {
    const fileName = getMacDmgFileName(version)
    const downloadUrl = `https://github.com/JaredP94/Personal-AI-Router/releases/download/v${version}/${fileName}`
    const updatesDir = getMacUpdatesDir()

    fs.mkdirSync(updatesDir, { recursive: true })
    const destPath = path.join(updatesDir, fileName)
    const tmpPath = `${destPath}.downloading`

    log.info(`[mac-updater] Downloading update from ${downloadUrl} to ${destPath}`)
    await fetchBinaryFile(downloadUrl, tmpPath, onProgress)

    fs.renameSync(tmpPath, destPath)
    writePendingUpdateVersion(version)
    log.info(`[mac-updater] Download complete: ${destPath}`)

    return destPath
}

export async function quitAndInstallMacUpdate(version: string): Promise<void> {
    const dmgPath = getCachedMacDmgPath(version)
    if (!dmgPath) {
        throw new Error(`Downloaded disk image for version ${version} not found.`)
    }

    const appPath = path.resolve(process.execPath, '../../..')
    if (!appPath.endsWith('.app') || !fs.existsSync(appPath)) {
        throw new Error(`Unable to resolve application bundle from ${process.execPath}`)
    }

    log.info(`[mac-updater] Preparing detached installer for ${appPath} using ${dmgPath}`)

    const tempDir = app.getPath('temp')
    try {
        fs.mkdirSync(tempDir, { recursive: true })
    } catch {
        /* best-effort */
    }
    const scriptPath = path.join(tempDir, `nvpair-update-${Date.now()}.sh`)
    const scriptContent = `#!/usr/bin/env bash
set -e

APP_PID=$1
DMG_PATH="$2"
DEST_APP="$3"
SCRIPT_PATH="$4"

COUNT=0
while kill -0 "$APP_PID" 2>/dev/null; do
    sleep 0.5
    COUNT=$((COUNT + 1))
    if [ "$COUNT" -ge 40 ]; then
        kill -9 "$APP_PID" 2>/dev/null || true
        break
    fi
done
sleep 1

TMP_MOUNT=$(mktemp -d /tmp/nvpair-update.XXXXXX)

cleanup() {
    if [ -n "$TMP_MOUNT" ] && [ -d "$TMP_MOUNT" ]; then
        hdiutil detach "$TMP_MOUNT" -quiet 2>/dev/null || hdiutil detach "$TMP_MOUNT" -force -quiet 2>/dev/null || true
        rm -rf "$TMP_MOUNT"
    fi
    rm -f "$DMG_PATH"
    rm -f "$SCRIPT_PATH"
}
trap cleanup EXIT INT TERM

hdiutil attach "$DMG_PATH" -mountpoint "$TMP_MOUNT" -nobrowse -readonly -quiet

APP_SRC="$TMP_MOUNT/PAIR.app"
if [ ! -d "$APP_SRC" ]; then
    APP_SRC=$(find "$TMP_MOUNT" -maxdepth 1 -name "*.app" | head -n1)
fi

if [ -z "$APP_SRC" ] || [ ! -d "$APP_SRC" ]; then
    exit 1
fi

rm -rf "$DEST_APP"
cp -R "$APP_SRC" "$DEST_APP"

if [ -d "$DEST_APP/Contents/Resources/cli-bin" ]; then
    chmod -R +x "$DEST_APP/Contents/Resources/cli-bin" 2>/dev/null || true
fi
if [ -d "$DEST_APP/Contents/MacOS" ]; then
    chmod -R +x "$DEST_APP/Contents/MacOS" 2>/dev/null || true
fi

xattr -cr "$DEST_APP" 2>/dev/null || true

hdiutil detach "$TMP_MOUNT" -quiet || hdiutil detach "$TMP_MOUNT" -force -quiet || true
TMP_MOUNT=""

open "$DEST_APP"
`

    fs.writeFileSync(scriptPath, scriptContent, { mode: 0o755 })

    const child = spawn(
        '/bin/bash',
        [scriptPath, String(process.pid), dmgPath, appPath, scriptPath],
        {
            detached: true,
            stdio: 'ignore'
        }
    )
    child.unref()

    clearPendingUpdate()
    try {
        await destroyConnector({ force: true })
    } catch (err) {
        log.warn(`[mac-updater] Connector cleanup failed: ${getErrorString(err)}`)
    }
    setTimeout(() => {
        app.exit(0)
    }, 5000).unref()
    app.quit()
}
