// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import {
    getFirewallSetupMode,
    runMacHelperSetup,
    type MacHelperSetupDeps
} from '@/electron/services/mac-helper-setup'
import type { EnsureConfiguredResult } from '@/electron/services/mac-privilege-service'

interface FakeCalls {
    isSupported: number
    trust: number
    isSetupComplete: number
    setSetupComplete: boolean[]
    ensureConfigured: boolean[]
    explainFirstRun: number
    explainApproval: number
}

interface FakeDepsOptions {
    supported?: boolean
    trustResult?: boolean | null
    setupComplete?: boolean
    ensureResult?: EnsureConfiguredResult
}

function createFakeDeps(options: FakeDepsOptions = {}): {
    deps: MacHelperSetupDeps
    calls: FakeCalls
} {
    let setupComplete = options.setupComplete ?? false
    const supported = options.supported ?? true
    const trustResult: boolean | null =
        options.trustResult !== undefined ? options.trustResult : true
    const ensureResult: EnsureConfiguredResult = options.ensureResult ?? {
        supported: true,
        registration: 'enabled',
        firewallConfigured: true,
        firewallError: null
    }

    const setSetupCompleteCalls: boolean[] = []
    const ensureConfiguredCalls: boolean[] = []
    const calls: FakeCalls = {
        isSupported: 0,
        trust: 0,
        isSetupComplete: 0,
        setSetupComplete: setSetupCompleteCalls,
        ensureConfigured: ensureConfiguredCalls,
        explainFirstRun: 0,
        explainApproval: 0
    }

    const deps: MacHelperSetupDeps = {
        isSupported: () => {
            calls.isSupported++
            return supported
        },
        trust: async () => {
            calls.trust++
            return trustResult
        },
        isSetupComplete: () => {
            calls.isSetupComplete++
            return setupComplete
        },
        setSetupComplete: (value: boolean) => {
            calls.setSetupComplete.push(value)
            setupComplete = value
        },
        ensureConfigured: async (forceFirewall: boolean) => {
            calls.ensureConfigured.push(forceFirewall)
            return ensureResult
        },
        explainFirstRun: async () => {
            calls.explainFirstRun++
        },
        explainApproval: async () => {
            calls.explainApproval++
        }
    }

    return { deps, calls }
}

describe('macHelperSetup', () => {
    it('untrusted build skips dialogs and helper install', async () => {
        const { deps, calls } = createFakeDeps({ trustResult: false })

        const mode = await runMacHelperSetup(deps)

        expect(mode).toBe('manual-unsigned-build')
        expect(getFirewallSetupMode()).toBe('manual-unsigned-build')
        expect(calls.explainFirstRun).toBe(0)
        expect(calls.ensureConfigured.length).toBe(0)
        expect(calls.setSetupComplete.length).toBe(0)
        expect(calls.explainApproval).toBe(0)
    })

    it('unknown trust does not prompt', async () => {
        const { deps, calls } = createFakeDeps({ trustResult: null })

        const mode = await runMacHelperSetup(deps)

        expect(mode).toBe('unknown')
        expect(getFirewallSetupMode()).toBe('unknown')
        expect(calls.explainFirstRun).toBe(0)
        expect(calls.ensureConfigured.length).toBe(0)
        expect(calls.setSetupComplete.length).toBe(0)
        expect(calls.explainApproval).toBe(0)
    })

    it('trusted first run keeps the existing flow', async () => {
        const { deps, calls } = createFakeDeps({
            setupComplete: false,
            trustResult: true,
            ensureResult: {
                supported: true,
                registration: 'enabled',
                firewallConfigured: true,
                firewallError: null
            }
        })

        const mode = await runMacHelperSetup(deps)

        expect(mode).toBe('automatic')
        expect(getFirewallSetupMode()).toBe('automatic')
        expect(calls.explainFirstRun).toBe(1)
        expect(calls.ensureConfigured).toEqual([true])
        expect(calls.setSetupComplete).toEqual([true])
        expect(calls.explainApproval).toBe(0)
    })

    it('approval pending explains only on first run', async () => {
        // First run: explainApproval called
        const firstRun = createFakeDeps({
            setupComplete: false,
            trustResult: true,
            ensureResult: {
                supported: true,
                registration: 'requiresApproval',
                firewallConfigured: false,
                firewallError: null
            }
        })

        const modeFirstRun = await runMacHelperSetup(firstRun.deps)

        expect(modeFirstRun).toBe('approval-required')
        expect(getFirewallSetupMode()).toBe('approval-required')
        expect(firstRun.calls.explainFirstRun).toBe(1)
        expect(firstRun.calls.ensureConfigured).toEqual([true])
        expect(firstRun.calls.explainApproval).toBe(1)
        expect(firstRun.calls.setSetupComplete.length).toBe(0)

        // Subsequent run: explainApproval not called
        const subsequentRun = createFakeDeps({
            setupComplete: true,
            trustResult: true,
            ensureResult: {
                supported: true,
                registration: 'requiresApproval',
                firewallConfigured: false,
                firewallError: null
            }
        })

        const modeSubsequent = await runMacHelperSetup(subsequentRun.deps)

        expect(modeSubsequent).toBe('approval-required')
        expect(getFirewallSetupMode()).toBe('approval-required')
        expect(subsequentRun.calls.explainFirstRun).toBe(0)
        expect(subsequentRun.calls.ensureConfigured).toEqual([false])
        expect(subsequentRun.calls.explainApproval).toBe(0)
        expect(subsequentRun.calls.setSetupComplete.length).toBe(0)
    })

    it('unsupported platform', async () => {
        const { deps, calls } = createFakeDeps({ supported: false })

        const mode = await runMacHelperSetup(deps)

        expect(mode).toBe('not-applicable')
        expect(getFirewallSetupMode()).toBe('not-applicable')
        expect(calls.trust).toBe(0)
        expect(calls.explainFirstRun).toBe(0)
        expect(calls.ensureConfigured.length).toBe(0)
        expect(calls.setSetupComplete.length).toBe(0)
        expect(calls.explainApproval).toBe(0)
    })

    it('incomplete helper setup returns unknown', async () => {
        const { deps, calls } = createFakeDeps({
            setupComplete: false,
            trustResult: true,
            ensureResult: {
                supported: true,
                registration: 'enabled',
                firewallConfigured: false,
                firewallError: 'socketfilterfw failed'
            }
        })

        const mode = await runMacHelperSetup(deps)

        expect(mode).toBe('unknown')
        expect(getFirewallSetupMode()).toBe('unknown')
        expect(calls.explainFirstRun).toBe(1)
        expect(calls.ensureConfigured).toEqual([true])
        expect(calls.setSetupComplete.length).toBe(0)
    })

    it('recovers gracefully and returns unknown when dependency throws', async () => {
        const { deps } = createFakeDeps({
            trustResult: true
        })
        deps.ensureConfigured = async () => {
            throw new Error('unexpected ctl crash')
        }

        const mode = await runMacHelperSetup(deps)

        expect(mode).toBe('unknown')
        expect(getFirewallSetupMode()).toBe('unknown')
    })
})
