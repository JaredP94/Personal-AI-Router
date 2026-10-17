// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { firewallSetupCopy } from '@/ui/utils/firewall-setup-copy'

describe('firewallSetupCopy', () => {
    it('returns manual copy and yellow tone for manual-unsigned-build', () => {
        expect(firewallSetupCopy('manual-unsigned-build')).toEqual({
            title: 'Firewall setup: manual',
            detail: "This unsigned build can't install the background helper that configures the macOS firewall. If macOS asks whether PAIR's services may accept incoming connections, choose Allow on networks you trust. Other nodes can't reach this one until you do.",
            tone: 'yellow'
        })
    })

    it('returns approval copy and yellow tone for approval-required', () => {
        expect(firewallSetupCopy('approval-required')).toEqual({
            title: 'Firewall setup: waiting for approval',
            detail: 'Open System Settings > General > Login Items & Extensions, enable the PAIR background item, then relaunch PAIR.',
            tone: 'yellow'
        })
    })

    it('returns automatic copy and green tone for automatic', () => {
        expect(firewallSetupCopy('automatic')).toEqual({
            title: 'Firewall setup: automatic',
            detail: 'The PAIR helper configured the macOS firewall.',
            tone: 'green'
        })
    })

    it('returns unknown copy and gray tone for unknown', () => {
        expect(firewallSetupCopy('unknown')).toEqual({
            title: 'Firewall setup: unknown',
            detail: "PAIR couldn't check the helper. Collect logs from this page if other nodes can't reach this machine.",
            tone: 'gray'
        })
    })

    it('returns null for not-applicable', () => {
        expect(firewallSetupCopy('not-applicable')).toBeNull()
    })
})
