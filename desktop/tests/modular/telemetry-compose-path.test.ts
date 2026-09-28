// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import fs from 'fs'
import path from 'path'
import { describe, expect, it } from 'vitest'
import { resolveTelemetryComposePath } from '@/electron/service-bridge/modular-supervisor'

describe('telemetry compose file resolution and packaging', () => {
    it('resolves the repository docker-compose.telemetry.yml file', () => {
        const resolved = resolveTelemetryComposePath()
        expect(resolved).not.toBe('')
        expect(fs.existsSync(resolved)).toBe(true)
        expect(path.basename(resolved)).toBe('docker-compose.telemetry.yml')
    })

    it('contains arizephoenix service definition in the resolved compose file', () => {
        const resolved = resolveTelemetryComposePath()
        const content = fs.readFileSync(resolved, 'utf8')
        expect(content).toContain('image: arizephoenix/phoenix:latest')
        expect(content).toContain('nvpair-phoenix')
    })

    it('ships docker-compose.telemetry.yml and telemetry-phoenix.sh in electron-builder extraResources', () => {
        const configPath = path.resolve(process.cwd(), 'electron-builder.config.ts')
        const content = fs.readFileSync(configPath, 'utf8')
        expect(content).toContain("'../docker-compose.telemetry.yml'")
        expect(content).toContain("'docker-compose.telemetry.yml'")
        expect(content).toContain("'../scripts/telemetry-phoenix.sh'")
        expect(content).toContain("'scripts/telemetry-phoenix.sh'")
    })
})
