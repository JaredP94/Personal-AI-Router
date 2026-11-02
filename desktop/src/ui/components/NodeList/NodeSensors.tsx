// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState, type ReactNode } from 'react'
import { Button, Flex, Text } from '@nvidia/foundations-react-core'
import type { MacmonInstallOutcome, NodeSensors } from '@/shared/types/metrics'
import type { NodeItem } from '@/shared/types/nodes'
import { Check, ContentCopy } from '@/ui/components/icons'
import { useDiscoveredNodesStore } from '@/ui/stores/discovered-nodes.store'
import { formatBytes } from '@/ui/utils/formatters'
import { memoryTone, sensorToneColors, type SensorTone } from '@/ui/utils/sensor-levels'

const INSTALL_COMMAND = 'brew install macmon'

function temperature(value: number | null): string {
    return value === null ? '—' : `${Math.round(value)} °C`
}

function watts(value: number | null): string {
    return value === null ? '—' : String(Math.round(value))
}

function bytes(value: number | null): string {
    return value === null ? '—' : formatBytes(value, 1)
}

export default function NodeSensorsPanel({
    node,
    sensors,
    isLocal
}: {
    node: NodeItem
    sensors: NodeSensors
    isLocal: boolean
}) {
    const discoveredNodes = useDiscoveredNodesStore(state => state.nodes)
    const canInstall = isLocal || discoveredNodes.some(peer => peer.id === node.id && peer.trusted)
    const [installing, setInstalling] = useState(false)
    const [installOutcome, setInstallOutcome] = useState<MacmonInstallOutcome | null>(null)

    useEffect(() => {
        if (sensors.macmon === 'available' && installOutcome !== null) setInstallOutcome(null)
    }, [sensors.macmon, installOutcome])

    useEffect(() => {
        if (installOutcome !== 'installed') return
        const timer = setTimeout(() => setInstallOutcome(null), 30_000)
        return () => clearTimeout(timer)
    }, [installOutcome])

    const install = async () => {
        setInstalling(true)
        const outcome = await window.pairApi.nodes.installMacmon(node.id)
        setInstallOutcome(outcome)
        setInstalling(false)
    }

    const available = sensors.macmon === 'available'
    const temperatures = [sensors.cpuTempC, sensors.gpuTempC].filter(value => value !== null)
    const hottest = temperatures.length > 0 ? Math.max(...temperatures) : null
    const fans = available ? sensors.fans : null
    const power = available ? sensors.power : null
    const memory = sensors.memoryPressure
    const memoryLabel =
        memory === 'normal'
            ? 'Normal'
            : memory === 'warning'
              ? 'Warning'
              : memory === 'critical'
                ? 'Critical'
                : '—'

    let notice: ReactNode = null
    if (
        installOutcome === 'installed' &&
        (sensors.macmon === 'missing' || sensors.macmon === 'failed')
    ) {
        notice = 'Installed — waiting for readings…'
    } else if (sensors.macmon === 'failed') {
        notice = 'Sensors not responding'
    } else if (sensors.macmon === 'missing') {
        if (installOutcome === 'homebrew-missing') {
            notice = (
                <>
                    <span>
                        Install Homebrew, then run <code>{INSTALL_COMMAND}</code>
                    </span>
                    <CopyMacmonCommand />
                </>
            )
        } else if (installOutcome === 'unsupported-platform') {
            notice = "macmon isn't supported on this Mac"
        } else if (canInstall) {
            notice = (
                <>
                    <span>
                        {installOutcome === 'failed'
                            ? 'Install failed — '
                            : 'Sensors unavailable — '}
                    </span>
                    <Button kind="secondary" size="tiny" onClick={install} disabled={installing}>
                        {installing && (
                            <span
                                className="spinner-element"
                                role="status"
                                aria-label="Installing macmon"
                            />
                        )}
                        Install macmon
                    </Button>
                </>
            )
        } else {
            notice = (
                <>
                    <span>
                        Sensors unavailable — run <code>{INSTALL_COMMAND}</code> on this node
                    </span>
                    <CopyMacmonCommand />
                </>
            )
        }
    }

    return (
        <div className="mt-5">
            <div className="grid grid-cols-2 min-[1000px]:grid-cols-4 gap-3">
                <SensorTile
                    title="Temperature"
                    value={available ? temperature(hottest) : '—'}
                    detail={
                        available
                            ? `CPU ${temperature(sensors.cpuTempC)} · GPU ${temperature(sensors.gpuTempC)}`
                            : undefined
                    }
                />
                <SensorTile
                    title="Fans"
                    value={
                        fans === null
                            ? '—'
                            : fans.length === 0
                              ? 'No fans'
                              : `${Math.max(...fans.map(fan => fan.rpm)).toLocaleString('en-US')} rpm`
                    }
                    detail={
                        fans && fans.length > 0
                            ? fans.map((fan, index) => (
                                  <span key={index} title={fan.name} className="block">
                                      of {fan.maxRpm.toLocaleString('en-US')} rpm
                                  </span>
                              ))
                            : undefined
                    }
                />
                <SensorTile
                    title="Power"
                    value={power?.systemW == null ? '—' : `${watts(power.systemW)} W`}
                    detail={
                        available
                            ? `CPU ${watts(power?.cpuW ?? null)} · GPU ${watts(power?.gpuW ?? null)} · ANE ${watts(power?.aneW ?? null)} W`
                            : undefined
                    }
                />
                <SensorTile
                    title="Memory"
                    value={memoryLabel}
                    detail={`swap ${bytes(sensors.swapUsedBytes)} / ${bytes(sensors.swapTotalBytes)}`}
                    tone={memoryTone(memory)}
                />
            </div>
            {notice && (
                <Flex align="center" wrap="wrap" gap="2" className="mt-3" role="status">
                    <Text kind="body/regular/sm" className="text-subtle-color">
                        {notice}
                    </Text>
                </Flex>
            )}
        </div>
    )
}

function SensorTile({
    title,
    value,
    detail,
    tone = 'default'
}: {
    title: string
    value: string
    detail?: ReactNode
    tone?: SensorTone
}) {
    return (
        <section aria-label={title} className="min-w-0 rounded-lg border p-3">
            <Text kind="body/regular/sm" className="text-subtle-color">
                {title}
            </Text>
            <Text
                kind="body/semibold/lg"
                className="block tabular-nums"
                style={{ color: sensorToneColors[tone] }}
            >
                {value}
            </Text>
            {detail && (
                <Text kind="body/regular/sm" className="text-subtle-color tabular-nums">
                    {detail}
                </Text>
            )}
        </section>
    )
}

function CopyMacmonCommand() {
    const [copied, setCopied] = useState(false)
    useEffect(() => {
        if (!copied) return
        const timer = setTimeout(() => setCopied(false), 1500)
        return () => clearTimeout(timer)
    }, [copied])

    return (
        <Button
            kind="tertiary"
            size="tiny"
            aria-label="Copy command"
            title={copied ? 'Copied' : 'Copy command'}
            onClick={() => {
                window.windowApi.window.copyToClipboard(INSTALL_COMMAND).then(() => setCopied(true))
            }}
        >
            {copied ? <Check style={{ fontSize: 14 }} /> : <ContentCopy style={{ fontSize: 14 }} />}
        </Button>
    )
}
