// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { app, BrowserWindow, Menu, nativeImage, screen, Tray } from 'electron'
import { getTrayWindow, createTrayWindow, createOverviewWindow } from '@/electron/window'
import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import { wakeNodeInfoPoller } from '@/electron/service-bridge/node-info-poller'
import { startTrayBadge, type TrayBadgeSink } from '@/electron/tray-badge'
import { renderTrayBadgeMap, type TrayBadgeMap } from '@/shared/utils/tray-badge-map'
import { createStructuredLogger } from '@/shared/utils/log'
import { currentPlatform } from '@/shared/utils/platform'
import { APP_DISPLAY_NAME } from '@/shared/constants/app'

const log = createStructuredLogger('tray')

const TRAY_WINDOW_WIDTH = 380
const MIN_HEIGHT = 100
const MAX_HEIGHT_RATIO = 0.8
const MARGIN = 10
const BLUR_GRACE_MS = 500
const RETRY_DELAY = 1000
const MAX_RETRIES = 3
const VISIBILITY_CHECK_INTERVAL = 5000

class TrayManager {
    private tray: Tray | null = null
    private contextMenu: Menu | null = null
    private isVisible = false
    private retryCount = 0
    private currentHeight = MIN_HEIGHT
    private lastBlurHideAtMs = 0
    private lastShowAtMs = 0
    private lastTrayBounds: Electron.Rectangle | undefined
    private lastCursorPoint: Electron.Point | undefined
    private blurHidingDisabled = false
    private displayChangeListener: (() => void) | undefined
    private visibilityInterval: ReturnType<typeof setInterval> | undefined
    private stopBadge: (() => void) | null = null
    private lastBadgeMap: TrayBadgeMap | null = null

    /**
     * The icon, drawn rather than loaded.
     *
     * `resources/icons/logo.png` is the full-colour app icon: a filled pentagon whose
     * node glyph is dark paint, not transparency. Set as a macOS template image it
     * flattens to a solid block, which is the white square this replaces. The badge
     * map carries real holes, so template mode has something to draw.
     */
    private toNativeImage(map: TrayBadgeMap): Electron.NativeImage {
        const bitmap = Buffer.from(map.pixels.buffer, map.pixels.byteOffset, map.pixels.byteLength)
        const image = nativeImage.createFromBitmap(bitmap, {
            width: map.width,
            height: map.height,
            scaleFactor: map.scaleFactor
        })
        if (currentPlatform() === 'darwin') {
            try {
                image.setTemplateImage(true)
            } catch {
                /* not supported in test env */
            }
            return image
        }

        // `scaleFactor` is honoured on macOS only, so the 2× bitmap would otherwise
        // arrive 32 pixels wide on a 22-pixel panel and a 16-pixel small-icon slot.
        const size = currentPlatform() === 'win32' ? 16 : 22
        return image.resize({ width: size, height: size, quality: 'best' })
    }

    /**
     * Red for an alert only where the icon is rendered verbatim. macOS paints a
     * template image in the menu bar's own colour, so there the `!` shape is the
     * whole message.
     */
    private alertInk(): { r: number; g: number; b: number } | undefined {
        return currentPlatform() === 'darwin' ? undefined : { r: 255, g: 64, b: 64 }
    }

    private applyBadgeMap(map: TrayBadgeMap): void {
        this.lastBadgeMap = map
        if (!this.tray) return
        try {
            this.tray.setImage(this.toNativeImage(map))
        } catch (error) {
            log.error({
                sublevel: 'lifecycle',
                message: `Failed to set tray icon: ${String(error)}`
            })
        }
    }

    private badgeSink(): TrayBadgeSink {
        return {
            setIcon: map => this.applyBadgeMap(map),
            setToolTip: text => {
                try {
                    this.tray?.setToolTip(text)
                } catch {
                    /* tray gone mid-repaint */
                }
            }
        }
    }

    async init(): Promise<void> {
        await app.whenReady()
        log.info({ sublevel: 'lifecycle', message: 'Initializing tray' })

        if (currentPlatform() === 'linux' && process.env.XDG_CURRENT_DESKTOP?.includes('Unity')) {
            if (!process.env.XDG_CURRENT_DESKTOP.includes('Unity7')) {
                process.env.XDG_CURRENT_DESKTOP = 'Unity7'
            }
        }

        try {
            const idle = renderTrayBadgeMap({ activeCount: 0, alert: false })
            this.tray = new Tray(this.toNativeImage(idle))
            this.lastBadgeMap = idle
            this.tray.setToolTip(APP_DISPLAY_NAME)

            this.setupContextMenu()
            this.setupClickHandlers()
            this.setupVisibilityCheck()
            this.setupDisplayChangeListeners()

            const win = createTrayWindow()
            this.setupTrayWindowEvents(win)

            // A recreated Tray is a new native object, so only the first init
            // subscribes; a recreation re-applies what the badge already decided.
            if (this.stopBadge === null) {
                this.stopBadge = startTrayBadge(
                    this.badgeSink(),
                    Object.values(getModularBridgeState().getWorkloads()),
                    { alertInk: this.alertInk() }
                )
            } else {
                this.applyBadgeMap(this.lastBadgeMap ?? idle)
            }

            this.isVisible = true
            this.retryCount = 0
            if (this.visibilityInterval) {
                clearInterval(this.visibilityInterval)
                this.visibilityInterval = undefined
            }
            log.info({ sublevel: 'lifecycle', message: 'Tray initialized successfully' })
        } catch (error) {
            log.error({ sublevel: 'lifecycle', message: `Failed to initialize tray: ${error}` })
            this.handleTrayError()
        }
    }

    private setupContextMenu(): void {
        this.contextMenu = Menu.buildFromTemplate([
            {
                label: 'Overview',
                click: () => this.showOrCreateMainWindow()
            },
            { type: 'separator' },
            {
                label: `Exit ${APP_DISPLAY_NAME}`,
                click: () => app.quit()
            }
        ])

        if (currentPlatform() !== 'darwin') {
            this.tray!.setContextMenu(this.contextMenu)
        }

        if (currentPlatform() === 'linux') {
            this.tray!.setIgnoreDoubleClickEvents(true)
        }
    }

    private setupClickHandlers(): void {
        if (!this.tray) return

        if (currentPlatform() === 'linux') {
            this.tray.on('click', (_event, bounds) => this.handlePrimaryClick(bounds))
            this.tray.on('middle-click', (_event, bounds) => this.handlePrimaryClick(bounds))
            this.tray.on('double-click', (_event, bounds) => this.handlePrimaryClick(bounds))
            this.tray.on('right-click', () => this.tray?.popUpContextMenu())
        } else if (currentPlatform() === 'darwin') {
            this.tray.on('click', (_event, bounds) => this.handlePrimaryClick(bounds))
            this.tray.on('right-click', () => {
                if (this.contextMenu) this.tray?.popUpContextMenu(this.contextMenu)
            })
        } else {
            this.tray.on('click', (_event, bounds) => this.handlePrimaryClick(bounds))
            this.tray.on('right-click', () => this.tray?.popUpContextMenu())
        }
    }

    private handlePrimaryClick = (bounds?: Electron.Rectangle): void => {
        if (bounds) this.lastTrayBounds = bounds
        try {
            this.lastCursorPoint = screen.getCursorScreenPoint()
        } catch {
            this.lastCursorPoint = undefined
        }

        let win = getTrayWindow()
        if (!win) {
            win = createTrayWindow()
            this.setupTrayWindowEvents(win)
        }

        if (win.isVisible()) {
            if (currentPlatform() === 'darwin') {
                win.hide()
            } else if (currentPlatform() === 'win32') {
                if (!win.isMinimized()) win.hide()
            } else {
                win.hide()
            }
            return
        }

        const blurAge = Date.now() - this.lastBlurHideAtMs
        if (blurAge < BLUR_GRACE_MS) return

        this.positionTrayWindow(win)

        this.blurHidingDisabled = true
        this.lastShowAtMs = Date.now()
        win.show()

        if (currentPlatform() === 'darwin') {
            app.focus()
            win.focus()
        } else if (currentPlatform() === 'win32') {
            win.focus()
        }

        setTimeout(() => {
            this.blurHidingDisabled = false
        }, 400)
    }

    private setupTrayWindowEvents(win: BrowserWindow): void {
        win.on('show', () => {
            wakeNodeInfoPoller()
            if (!win.isDestroyed() && !win.webContents.isDestroyed()) {
                win.webContents.send('tray:visibility', true)
            }
        })
        win.on('hide', () => {
            if (!win.isDestroyed() && !win.webContents.isDestroyed()) {
                win.webContents.send('tray:visibility', false)
            }
        })
        win.on('blur', () => {
            if (this.blurHidingDisabled) return
            const showAge = Date.now() - this.lastShowAtMs
            if (showAge < 300) return
            this.lastBlurHideAtMs = Date.now()
            win.hide()
        })
    }

    private positionTrayWindow(win: BrowserWindow): void {
        const trayBounds = this.lastTrayBounds ?? this.getFreshTrayBounds()
        const cursorPoint = this.lastCursorPoint

        if (currentPlatform() === 'darwin') {
            this.positionForMacOS(win, trayBounds, cursorPoint)
        } else if (currentPlatform() === 'win32') {
            this.positionForWindows(win)
        } else {
            this.positionForLinux(win, trayBounds)
        }
    }

    private positionForMacOS(
        win: BrowserWindow,
        trayBounds: Electron.Rectangle,
        cursorPoint?: Electron.Point
    ): void {
        const h = this.currentHeight
        const targetPoint = cursorPoint ?? { x: trayBounds.x, y: trayBounds.y }
        const display = screen.getDisplayNearestPoint(targetPoint)
        const workArea = display.workArea

        const anchorX = trayBounds.x + trayBounds.width / 2
        let x = Math.round(anchorX - TRAY_WINDOW_WIDTH / 2)
        let y = Math.round(trayBounds.y + trayBounds.height + MARGIN)

        x = Math.max(workArea.x, Math.min(x, workArea.x + workArea.width - TRAY_WINDOW_WIDTH))
        y = Math.max(workArea.y, Math.min(y, workArea.y + workArea.height - h))

        win.setBounds({ x, y, width: TRAY_WINDOW_WIDTH, height: h })
        win.setWindowButtonVisibility(false)
        win.setAlwaysOnTop(true, 'floating')
        win.setFullScreenable(false)
    }

    private positionForWindows(win: BrowserWindow): void {
        const h = this.currentHeight
        const workArea = screen.getPrimaryDisplay().workArea
        const x = Math.round(workArea.x + workArea.width - TRAY_WINDOW_WIDTH - MARGIN)
        const y = Math.round(workArea.y + workArea.height - h - MARGIN)

        win.setBounds({ x, y, width: TRAY_WINDOW_WIDTH, height: h })
    }

    private positionForLinux(win: BrowserWindow, trayBounds: Electron.Rectangle): void {
        const h = this.currentHeight
        const workArea = screen.getPrimaryDisplay().workArea
        const x = Math.round(workArea.x + workArea.width - TRAY_WINDOW_WIDTH - MARGIN)

        let y: number
        if (trayBounds.y > workArea.height * 0.75) {
            y = Math.round(trayBounds.y - h - MARGIN)
        } else if (trayBounds.y < workArea.height * 0.25) {
            y = Math.round(trayBounds.y + trayBounds.height + MARGIN)
        } else {
            y = MARGIN
        }

        y = Math.max(workArea.y, Math.min(y, workArea.y + workArea.height - h))
        win.setBounds({ x, y, width: TRAY_WINDOW_WIDTH, height: h })
    }

    private getFreshTrayBounds(): Electron.Rectangle {
        try {
            if (this.tray) return this.tray.getBounds()
        } catch {
            /* fall through */
        }
        const cursor = screen.getCursorScreenPoint()
        return { x: cursor.x - 10, y: cursor.y - 10, width: 20, height: 20 }
    }

    private setupDisplayChangeListeners(): void {
        if (this.displayChangeListener) {
            try {
                screen.removeListener('display-metrics-changed', this.displayChangeListener)
            } catch {
                /* ignore */
            }
        }

        this.displayChangeListener = () => {
            this.lastTrayBounds = undefined
            this.lastCursorPoint = undefined
        }

        try {
            screen.on('display-metrics-changed', this.displayChangeListener)
        } catch {
            /* ignore */
        }
    }

    private setupVisibilityCheck(): void {
        if (this.visibilityInterval) clearInterval(this.visibilityInterval)
        this.visibilityInterval = setInterval(() => {
            if (!this.isVisible && this.retryCount < MAX_RETRIES) {
                log.warn({ sublevel: 'lifecycle', message: 'Tray not visible, recreating' })
                this.recreateTray()
            } else if (this.visibilityInterval) {
                clearInterval(this.visibilityInterval)
                this.visibilityInterval = undefined
            }
        }, VISIBILITY_CHECK_INTERVAL)
    }

    private handleTrayError(): void {
        if (this.retryCount < MAX_RETRIES) {
            this.retryCount++
            log.info({
                sublevel: 'lifecycle',
                message: `Retrying tray init (attempt ${this.retryCount}/${MAX_RETRIES})`
            })
            setTimeout(() => this.init(), RETRY_DELAY)
        } else {
            log.error({ sublevel: 'lifecycle', message: 'Failed to init tray after max retries' })
        }
    }

    private recreateTray(): void {
        try {
            if (this.tray) this.tray.destroy()
        } catch {
            /* ignore */
        }
        this.init()
    }

    resizeTrayWindow(contentHeight: number): number {
        const maxHeight = Math.ceil(screen.getPrimaryDisplay().workArea.height * MAX_HEIGHT_RATIO)
        const height = Math.max(MIN_HEIGHT, Math.min(Math.ceil(contentHeight), maxHeight))
        this.currentHeight = height

        const win = getTrayWindow()
        if (win && !win.isDestroyed() && win.isVisible()) {
            win.setResizable(true)
            this.positionTrayWindow(win)
            win.setResizable(false)
        }

        return maxHeight
    }

    showOrCreateMainWindow(): void {
        createOverviewWindow()
    }

    destroy(): void {
        log.info({ sublevel: 'lifecycle', message: 'Destroying tray' })
        this.stopBadge?.()
        this.stopBadge = null
        if (this.visibilityInterval) clearInterval(this.visibilityInterval)
        if (this.displayChangeListener) {
            try {
                screen.removeListener('display-metrics-changed', this.displayChangeListener)
            } catch {
                /* ignore */
            }
        }
        const win = getTrayWindow()
        if (win && !win.isDestroyed()) win.destroy()
        if (this.tray) {
            this.tray.destroy()
            this.tray = null
        }
    }
}

const trayManager = new TrayManager()

export function initTray(): Promise<void> {
    return trayManager.init()
}

export function destroyTray(): void {
    trayManager.destroy()
}

export function resizeTrayWindow(contentHeight: number): number {
    return trayManager.resizeTrayWindow(contentHeight)
}
