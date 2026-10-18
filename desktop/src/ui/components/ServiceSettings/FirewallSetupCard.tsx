// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react'
import { Badge, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import type { FirewallSetupMode } from '@/shared/types/ipc-channels'
import { isElectron } from '@/ui/api/bootstrap'
import { firewallSetupCopy } from '@/ui/utils/firewall-setup-copy'

export default function FirewallSetupCard() {
    const [mode, setMode] = useState<FirewallSetupMode | null>(null)

    useEffect(() => {
        if (!isElectron) return
        window.windowApi.service
            .getFirewallSetup()
            .then(setMode)
            .catch(() => {})
    }, [])

    if (!mode) return null
    const copy = firewallSetupCopy(mode)
    if (!copy) return null

    return (
        <div className="settings-card settings-card-stacked pair-paper p-4">
            <Stack gap="2">
                <Flex align="center" gap="3" wrap="wrap">
                    <Text kind="body/semibold/md">{copy.title}</Text>
                    <Badge color={copy.tone} kind="solid">
                        macOS
                    </Badge>
                </Flex>
                <Text kind="body/regular/sm" className="text-subtle-color">
                    {copy.detail}
                </Text>
            </Stack>
        </div>
    )
}
