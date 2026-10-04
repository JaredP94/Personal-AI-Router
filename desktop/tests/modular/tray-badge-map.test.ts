// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import {
    TRAY_GLYPH_SIZE,
    TRAY_GLYPHS,
    TRAY_ICON_SCALE,
    TRAY_ICON_SIZE,
    TRAY_MARK_BARE,
    TRAY_MARK_IDLE,
    renderTrayBadgeMap
} from '@/shared/utils/tray-badge-map'
import type { TrayBadge } from '@/shared/utils/tray-badge-state'

/**
 * The menu bar icon shipped as a macOS *template* image built from the full-colour
 * app icon. Template rendering keeps only the alpha channel and paints every
 * opaque pixel one flat colour, so the mark — a filled pentagon whose node glyph
 * is drawn in dark paint rather than cut out as transparency — flattened into a
 * solid white block. These tests pin what the replacement must never lose.
 *
 * The numeral lives inside the pentagon, which is why the invariants below are
 * about the frame and the glyph band rather than about a badge sitting beside it.
 */

const IDLE: TrayBadge = { activeCount: 0, alert: false }

function map(badge: TrayBadge, ink?: { r: number; g: number; b: number }) {
    return renderTrayBadgeMap(badge, ink ? { ink } : undefined)
}

function alpha(m: ReturnType<typeof map>, x: number, y: number): number {
    return m.pixels[(y * TRAY_ICON_SCALE * m.width + x * TRAY_ICON_SCALE) * 4 + 3]
}

function opaqueCount(m: ReturnType<typeof map>): number {
    let opaque = 0
    for (let i = 3; i < m.pixels.length; i += 4) {
        if (m.pixels[i] > 127) opaque++
    }
    return opaque
}

function opaqueFraction(m: ReturnType<typeof map>): number {
    return opaqueCount(m) / (m.width * m.height)
}

function fingerprint(m: ReturnType<typeof map>): string {
    let hash = 0
    for (let i = 3; i < m.pixels.length; i += 4) {
        hash = (hash * 31 + (m.pixels[i] > 127 ? 1 : 0)) | 0
    }
    return `${hash}:${opaqueCount(m)}`
}

/** The rows the numeral occupies; the idle glyph shares this band. */
function bandFingerprint(m: ReturnType<typeof map>): string {
    let hash = 0
    for (let y = 3; y <= 11; y++) {
        for (let x = 0; x < 16; x++) {
            hash = (hash * 31 + (alpha(m, x, y) > 127 ? 1 : 0)) | 0
        }
    }
    return String(hash)
}

describe('menu bar badge bitmap', () => {
    it('emits a BGRA buffer sized for a retina menu bar', () => {
        const m = map(IDLE)

        expect(m.scaleFactor).toBe(TRAY_ICON_SCALE)
        expect(m.width).toBe(32)
        expect(m.height).toBe(32)
        expect(m.pixels.length).toBe(m.width * m.height * 4)
    })

    it('leaves holes in the mark so template mode is not a solid block', () => {
        expect(opaqueCount(map(IDLE))).toBeGreaterThan(0)
        expect(opaqueFraction(map(IDLE))).toBeLessThan(0.8)
    })

    it('keeps the pentagon frame whatever the icon says', () => {
        for (const badge of [
            IDLE,
            { activeCount: 3, alert: false },
            { activeCount: 0, alert: true }
        ]) {
            const m = map(badge)

            for (let x = 2; x <= 13; x++) {
                expect(alpha(m, x, 14)).toBe(255)
            }
            expect(alpha(m, 7, 1)).toBe(255)
        }
    })

    it('shows the glyph only while there is nothing to report', () => {
        const idle = bandFingerprint(map(IDLE))

        expect(bandFingerprint(map({ activeCount: 1, alert: false }))).not.toBe(idle)
        expect(bandFingerprint(map({ activeCount: 0, alert: true }))).not.toBe(idle)
    })

    it('paints every opaque pixel with one flat ink colour', () => {
        const m = map({ activeCount: 3, alert: false })

        for (let i = 0; i < m.pixels.length; i += 4) {
            if (m.pixels[i + 3] === 0) continue
            expect(rgbAt(m, i)).toEqual([255, 255, 255])
        }
    })

    it('renders each digit 1 through 9 as a distinct glyph', () => {
        const seen = new Set<string>()

        for (let count = 1; count <= 9; count++) {
            seen.add(bandFingerprint(map({ activeCount: count, alert: false })))
        }

        expect(seen.size).toBe(9)
    })

    it('renders nine plus for a count above the cap', () => {
        expect(fingerprint(map({ activeCount: 42, alert: false }))).not.toBe(
            fingerprint(map({ activeCount: 9, alert: false }))
        )
    })

    it('shows the same alert glyph whatever the count', () => {
        const alertIdle = fingerprint(map({ activeCount: 0, alert: true }))

        expect(fingerprint(map({ activeCount: 7, alert: true }))).toBe(alertIdle)
        expect(alertIdle).not.toBe(fingerprint(map({ activeCount: 7, alert: false })))
    })

    it('keeps the numeral inside the pentagon', () => {
        const m = map({ activeCount: 42, alert: false })

        // The widest label is `9+`; its strokes must not touch the frame.
        for (let y = 3; y <= 11; y++) {
            expect(alpha(m, 0, y)).toBe(0)
            expect(alpha(m, 15, y)).toBe(0)
        }
    })

    it('lets a platform pass its own ink colour, as BGRA', () => {
        const red = map({ activeCount: 1, alert: true }, { r: 255, g: 0, b: 0 })
        let checked = 0

        for (let i = 0; i < red.pixels.length; i += 4) {
            if (red.pixels[i + 3] === 0) continue
            expect(rgbAt(red, i)).toEqual([255, 0, 0])
            checked++
        }

        expect(checked).toBeGreaterThan(0)
    })

    it('keeps every pixel row exactly as wide as the renderer reads it', () => {
        // A row authored one character short silently shifts everything after it,
        // which is how a mark becomes a blob.
        const marks = [TRAY_MARK_IDLE, TRAY_MARK_BARE]
        for (const rows of marks) {
            expect(rows).toHaveLength(TRAY_ICON_SIZE)
            for (const row of rows) expect(row).toHaveLength(TRAY_ICON_SIZE)
        }

        for (const glyph of Object.values(TRAY_GLYPHS)) {
            expect(glyph).toHaveLength(TRAY_GLYPH_SIZE.height)
            for (const row of glyph) expect(row).toHaveLength(TRAY_GLYPH_SIZE.width)
        }
    })
})

/**
 * Colour of the pixel starting at byte `i`, in R/G/B. `nativeImage.createFromBitmap`
 * requires BGRA, so the channels read reversed here — a renderer that wrote RGBA
 * would pass a colour-blind test and ship a blue badge where the platform asked
 * for red.
 */
function rgbAt(m: ReturnType<typeof map>, i: number): number[] {
    return [m.pixels[i + 2], m.pixels[i + 1], m.pixels[i]]
}
