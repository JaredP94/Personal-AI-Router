// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { FirewallSetupMode } from '@/shared/types/ipc-channels'

export function firewallSetupCopy(
    mode: FirewallSetupMode
): { title: string; detail: string; tone: 'green' | 'yellow' | 'gray' } | null {
    switch (mode) {
        case 'manual-unsigned-build':
            return {
                title: 'Firewall setup: manual',
                detail: "This unsigned build can't install the background helper that configures the macOS firewall. If macOS asks whether PAIR's services may accept incoming connections, choose Allow on networks you trust. Other nodes can't reach this one until you do.",
                tone: 'yellow'
            }
        case 'approval-required':
            return {
                title: 'Firewall setup: waiting for approval',
                detail: 'Open System Settings > General > Login Items & Extensions, enable the PAIR background item, then relaunch PAIR.',
                tone: 'yellow'
            }
        case 'automatic':
            return {
                title: 'Firewall setup: automatic',
                detail: 'The PAIR helper configured the macOS firewall.',
                tone: 'green'
            }
        case 'unknown':
            return {
                title: 'Firewall setup: unknown',
                detail: "PAIR couldn't check the helper. Collect logs from this page if other nodes can't reach this machine.",
                tone: 'gray'
            }
        case 'not-applicable':
            return null
    }
}
