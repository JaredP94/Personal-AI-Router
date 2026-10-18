// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { FirewallSetupMode } from '@/shared/types/ipc-channels'
import type { EnsureConfiguredResult } from '@/electron/services/mac-privilege-service'
import { createStructuredLogger } from '@/shared/utils/log'

const log = createStructuredLogger('mac-helper')

export interface MacHelperSetupDeps {
    isSupported(): boolean
    trust(): Promise<boolean | null>
    isSetupComplete(): boolean
    setSetupComplete(value: boolean): void
    ensureConfigured(forceFirewall: boolean): Promise<EnsureConfiguredResult>
    explainFirstRun(): Promise<void>
    explainApproval(): Promise<void>
}

let firewallSetupMode: FirewallSetupMode = 'not-applicable'

export function getFirewallSetupMode(): FirewallSetupMode {
    return firewallSetupMode
}

export async function runMacHelperSetup(deps: MacHelperSetupDeps): Promise<FirewallSetupMode> {
    try {
        if (!deps.isSupported()) {
            firewallSetupMode = 'not-applicable'
            return 'not-applicable'
        }

        const trusted = await deps.trust()
        if (trusted === false) {
            log.info({
                sublevel: 'mac-helper',
                message:
                    'Privileged helper signing trust not satisfied (unsigned build); skipping automated firewall setup'
            })
            firewallSetupMode = 'manual-unsigned-build'
            return 'manual-unsigned-build'
        }

        if (trusted === null) {
            log.error({
                sublevel: 'mac-helper',
                message: 'Failed to verify privileged helper signing trust'
            })
            firewallSetupMode = 'unknown'
            return 'unknown'
        }

        const firstTime = !deps.isSetupComplete()
        if (firstTime) {
            await deps.explainFirstRun()
        }

        const result = await deps.ensureConfigured(firstTime)
        if (!result.supported) {
            firewallSetupMode = 'unknown'
            return 'unknown'
        }

        if (result.registration === 'requiresApproval') {
            if (firstTime) {
                await deps.explainApproval()
            }
            log.info({
                sublevel: 'mac-helper',
                message: 'Privileged helper requires approval in System Settings'
            })
            firewallSetupMode = 'approval-required'
            return 'approval-required'
        }

        if (result.registration === 'enabled' && result.firewallConfigured) {
            deps.setSetupComplete(true)
            log.info({ sublevel: 'mac-helper', message: 'Privileged helper configured' })
            firewallSetupMode = 'automatic'
            return 'automatic'
        }

        log.error({
            sublevel: 'mac-helper',
            message: `Privileged helper setup incomplete: ${result.registration} ${result.firewallError ?? ''}`
        })
        firewallSetupMode = 'unknown'
        return 'unknown'
    } catch (err) {
        log.error({ sublevel: 'mac-helper', message: `Privileged setup error: ${err}` })
        firewallSetupMode = 'unknown'
        return 'unknown'
    }
}
