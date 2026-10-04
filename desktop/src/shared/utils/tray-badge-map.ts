// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * The PAIR menu bar icon, drawn as pixels.
 *
 * macOS renders a tray *template* image by keeping only its alpha channel and
 * painting every opaque pixel one flat colour, so the icon tracks the light and
 * dark menu bar. The app icon that shipped here is a filled green pentagon whose
 * node glyph is drawn in dark paint rather than cut out as transparency, so
 * template mode flattened it into a solid white block. This module draws the mark
 * the other way round: opaque strokes, transparent holes.
 *
 * The numeral goes *inside* the pentagon. The mark's interior is empty space at
 * this size, and a badge beside a 16-point icon has no room to be legible — the
 * frame and the glyph band are the only real estate there is.
 *
 * Pure: no Electron, no canvas, no rasterizer. It returns the BGRA bytes
 * `nativeImage.createFromBitmap` takes, which keeps the glyph geometry testable
 * and keeps a dependency off the desktop build.
 */
import { TRAY_BADGE_MAX_COUNT, type TrayBadge } from '@/shared/utils/tray-badge-state'

/** Logical (point) size of the menu bar icon. */
export const TRAY_ICON_SIZE = 16

/** Rendered at 2× so numerals keep their shape on a retina menu bar. */
export const TRAY_ICON_SCALE = 2

/** First row of the band the numeral is drawn in. */
const LABEL_TOP_ROW = 6

/** Width and height of every numeral cell in the label font. */
export const TRAY_GLYPH_SIZE = { width: 5, height: 6 }

/**
 * The pentagon with its node glyph: nodes four pixels wide joined to the hub by
 * a two-pixel stem. The waist is what makes a node read as a node at this size.
 */
export const TRAY_MARK_IDLE: readonly string[] = [
    '................',
    '.......##.......',
    '......#..#......',
    '....#.####.#....',
    '...#..####..#...',
    '..#....##....#..',
    '.#.....##.....#.',
    '.#....####....#.',
    '.#...#....#...#.',
    '.#..#......#..#.',
    '.#.####..####.#.',
    '.#.####..####.#.',
    '.#............#.',
    '.#............#.',
    '.##############.',
    '................'
]

/** The pentagon on its own — the frame the numeral is reported inside. */
export const TRAY_MARK_BARE: readonly string[] = [
    '................',
    '.......##.......',
    '......#..#......',
    '....#......#....',
    '...#........#...',
    '..#..........#..',
    '.#............#.',
    '.#............#.',
    '.#............#.',
    '.#............#.',
    '.#............#.',
    '.#............#.',
    '.#............#.',
    '.#............#.',
    '.##############.',
    '................'
]

/** Five-by-six numerals, the `+` of the cap, and the `!` of an alert. */
export const TRAY_GLYPHS: Record<string, readonly string[]> = {
    '0': ['.###.', '#...#', '#...#', '#...#', '#...#', '.###.'],
    '1': ['..#..', '.##..', '..#..', '..#..', '..#..', '.###.'],
    '2': ['.###.', '#...#', '....#', '..##.', '.#...', '#####'],
    '3': ['.###.', '....#', '..##.', '....#', '#...#', '.##..'],
    '4': ['...#.', '..##.', '.#.#.', '#..#.', '#####', '...#.'],
    '5': ['#####', '#....', '####.', '....#', '#...#', '.##..'],
    '6': ['..##.', '.#...', '####.', '#...#', '#...#', '.##..'],
    '7': ['#####', '....#', '...#.', '..#..', '..#..', '..#..'],
    '8': ['.###.', '#...#', '.###.', '#...#', '#...#', '.###.'],
    '9': ['.###.', '#...#', '#...#', '.####', '...#.', '..#..'],
    '+': ['.....', '..#..', '..#..', '#####', '..#..', '.....'],
    '!': ['..#..', '..#..', '..#..', '..#..', '.....', '..#..']
}

export interface TrayBadgeInk {
    r: number
    g: number
    b: number
}

export interface TrayBadgeMap {
    pixels: Uint8Array
    width: number
    height: number
    scaleFactor: number
}

/** White, because a template image is recoloured by the platform anyway. */
const DEFAULT_INK: TrayBadgeInk = { r: 255, g: 255, b: 255 }

/** Transparent pixel. */
const CLEAR = 0
/** Opaque pixel. */
const INK = 255

/**
 * What the icon reads. The alert replaces the count: one 16-point icon cannot
 * carry two glyphs, and a failure is the fact that matters.
 */
function badgeLabel(badge: TrayBadge): string {
    if (badge.alert) return '!'
    if (badge.activeCount <= 0) return ''
    if (badge.activeCount > TRAY_BADGE_MAX_COUNT) return `${TRAY_BADGE_MAX_COUNT}+`
    return String(badge.activeCount)
}

function paintRow(mark: readonly string[], row: number): string {
    return mark[row] ?? ''
}

/** Centre the label in the pentagon's interior. */
function stampLabel(grid: Uint8Array, label: string): void {
    const gap = 1
    const total = label.length * TRAY_GLYPH_SIZE.width + gap * Math.max(0, label.length - 1)
    const startX = Math.round((TRAY_ICON_SIZE - total) / 2)

    for (let index = 0; index < label.length; index++) {
        const glyph = TRAY_GLYPHS[label[index]]
        if (!glyph) continue
        for (let row = 0; row < TRAY_GLYPH_SIZE.height; row++) {
            for (let col = 0; col < TRAY_GLYPH_SIZE.width; col++) {
                if (glyph[row]?.[col] !== '#') continue
                const at =
                    (LABEL_TOP_ROW + row) * TRAY_ICON_SIZE +
                    startX +
                    col +
                    index * (TRAY_GLYPH_SIZE.width + gap)
                grid[at] = 1
            }
        }
    }
}

function paintMark(grid: Uint8Array, mark: readonly string[]): void {
    for (let row = 0; row < TRAY_ICON_SIZE; row++) {
        const source = paintRow(mark, row)
        for (let col = 0; col < TRAY_ICON_SIZE; col++) {
            if (source[col] === '#') grid[row * TRAY_ICON_SIZE + col] = 1
        }
    }
}

export function renderTrayBadgeMap(
    badge: TrayBadge,
    options?: { ink?: TrayBadgeInk }
): TrayBadgeMap {
    const ink = options?.ink ?? DEFAULT_INK
    const label = badgeLabel(badge)
    const mark = label === '' ? TRAY_MARK_IDLE : TRAY_MARK_BARE
    const grid = new Uint8Array(TRAY_ICON_SIZE * TRAY_ICON_SIZE)

    paintMark(grid, mark)
    stampLabel(grid, label)

    const width = TRAY_ICON_SIZE * TRAY_ICON_SCALE
    const height = TRAY_ICON_SIZE * TRAY_ICON_SCALE
    const pixels = new Uint8Array(width * height * 4)

    for (let y = 0; y < height; y++) {
        for (let x = 0; x < width; x++) {
            const logicalY = (y / TRAY_ICON_SCALE) | 0
            const logicalX = (x / TRAY_ICON_SCALE) | 0
            const set = grid[logicalY * TRAY_ICON_SIZE + logicalX] === 1
            const at = (y * width + x) * 4
            // BGRA, the byte order nativeImage.createFromBitmap requires.
            pixels[at] = ink.b
            pixels[at + 1] = ink.g
            pixels[at + 2] = ink.r
            pixels[at + 3] = set ? INK : CLEAR
        }
    }

    return { pixels, width, height, scaleFactor: TRAY_ICON_SCALE }
}
