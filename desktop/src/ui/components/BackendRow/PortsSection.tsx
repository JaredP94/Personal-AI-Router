// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Text } from '@nvidia/foundations-react-core'
import { BackendPorts } from './BackendPorts'
import { EditState } from '@/ui/types/engine-edit-state'

export function PortsSection({
    edit,
    portsChanged,
    anyLoading,
    isLocalNode,
    canSetPort,
    onApplyPorts,
    onServerChange,
    onProxyChange
}: {
    edit: EditState
    portsChanged: boolean
    anyLoading: boolean
    isLocalNode: boolean
    /**
     * Whether the engine's own server port can be edited, as reported by the
     * node that owns this engine (`EngineStatus.capabilities.set_port`). False
     * hides the field entirely — e.g. oMLX, whose start command takes no port.
     */
    canSetPort: boolean
    onApplyPorts: () => void
    onServerChange: (v: string) => void
    onProxyChange: (v: string) => void
}) {
    return (
        <details className="pair-accordion translucent-bg-accordion">
            <summary className="pair-accordion-summary">
                <Text kind="body/semibold/sm">Ports</Text>
            </summary>
            <div className="p-3">
                <BackendPorts
                    proxyPort={edit.proxyPort}
                    serverPort={edit.serverPort}
                    changed={portsChanged}
                    disabled={anyLoading}
                    isLocalNode={isLocalNode}
                    showServerPort={canSetPort}
                    onServerChange={onServerChange}
                    onProxyChange={onProxyChange}
                    onApply={onApplyPorts}
                />
            </div>
        </details>
    )
}
