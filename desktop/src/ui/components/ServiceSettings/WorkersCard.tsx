// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useState } from 'react'
import { Badge, Button, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import type { WorkerState, WorkerStatus } from '@/shared/types/workers'
import getErrorString from '@/shared/utils/get-error-string'
import { ConfirmModal } from '@/ui/components/ConfirmModal'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'
import { useWorkersStore } from '@/ui/stores/workers.store'

const STATE_BADGES: Record<
    WorkerState,
    { label: string; color: 'green' | 'yellow' | 'red' | 'gray' }
> = {
    running: { label: 'Running', color: 'green' },
    degraded: { label: 'Not responding', color: 'yellow' },
    restarting: { label: 'Restarting', color: 'yellow' },
    exhausted: { label: 'Gave up', color: 'red' },
    missing: { label: 'Not found', color: 'gray' }
}

/**
 * Restarting these interrupts work the user cannot see from the process table:
 * the engine manager owns the engines PAIR started, and each proxy serves live
 * inference over HTTP independently of the stdio loop a restart tears down.
 * Desktop asks first; whether the restart is allowed is the broker's call.
 */
const RESTART_WARNINGS: Record<string, string> = {
    'engine-manager':
        'Engines PAIR started may keep running or stop; running inference may be interrupted.',
    proxy: 'Running inference through this proxy will be interrupted.',
    'lmstudio-proxy': 'Running inference through this proxy will be interrupted.',
    'omlx-proxy': 'Running inference through this proxy will be interrupted.'
}

function WorkerRow({
    worker,
    busy,
    error,
    onRestart
}: {
    worker: WorkerStatus
    busy: boolean
    error: string | null
    onRestart: () => void
}) {
    const badge = STATE_BADGES[worker.state]
    // A degraded worker the broker never restarts on a hang (engine-manager and
    // the proxies) will sit like this until somebody acts, so say so.
    const manualRestart = worker.state === 'degraded' && !worker.restartOnHang

    return (
        <Stack gap="1">
            <Flex justify="between" align="center" gap="4" wrap="wrap">
                <Flex align="center" gap="3" wrap="wrap">
                    <Text kind="body/semibold/sm">{worker.name}</Text>
                    <Badge color={badge.color} kind="solid">
                        {badge.label}
                    </Badge>
                    {worker.pid !== undefined && (
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            {`PID ${worker.pid}`}
                        </Text>
                    )}
                </Flex>

                <Flex align="center" gap="3" wrap="wrap">
                    <Text kind="body/regular/sm" className="text-subtle-color">
                        {`Attempts: ${worker.attempts}`}
                    </Text>
                    <Button
                        kind="secondary"
                        size="small"
                        onClick={onRestart}
                        disabled={busy}
                        aria-label={`Restart ${worker.name}`}
                    >
                        Restart
                    </Button>
                </Flex>
            </Flex>

            {worker.lastError && (
                <Text kind="body/regular/sm" className="text-subtle-color wrap-break-word">
                    {worker.lastError}
                </Text>
            )}
            {manualRestart && (
                <Text kind="body/regular/sm" className="text-subtle-color">
                    Not responding — restart manually
                </Text>
            )}
            {error && <InlineErrorBanner message={error} />}
        </Stack>
    )
}

/**
 * Settings → Service → Workers: the broker-reported lifecycle of every worker
 * it supervises, and the one place a stuck worker can be restarted from. The
 * rows are rendered exactly as the broker reports them; desktop computes no
 * health, retry or restart policy of its own.
 */
export default function WorkersCard() {
    const workers = useWorkersStore(state => state.workers)
    const restartWorker = useWorkersStore(state => state.restartWorker)
    const [busy, setBusy] = useState<string | null>(null)
    const [errors, setErrors] = useState<Record<string, string>>({})
    const [confirming, setConfirming] = useState<WorkerStatus | null>(null)

    const run = async (worker: WorkerStatus): Promise<void> => {
        setBusy(worker.name)
        setErrors(current => {
            const next = { ...current }
            delete next[worker.name]
            return next
        })
        try {
            await restartWorker(worker.name)
        } catch (err) {
            const message = getErrorString(err)
            setErrors(current => ({ ...current, [worker.name]: message }))
        } finally {
            setBusy(current => (current === worker.name ? null : current))
        }
    }

    const requestRestart = (worker: WorkerStatus): void => {
        if (RESTART_WARNINGS[worker.name]) setConfirming(worker)
        else void run(worker)
    }

    return (
        <div className="settings-card settings-card-stacked pair-paper p-4">
            <Stack gap="4">
                <Flex align="center" gap="3" wrap="wrap">
                    <Text kind="body/semibold/md">Workers</Text>
                    <Text kind="body/regular/sm" className="text-subtle-color">
                        {workers.length === 0
                            ? 'No worker status reported yet.'
                            : 'Reported by the service, which owns retries and restarts.'}
                    </Text>
                </Flex>

                {workers.map(worker => (
                    <WorkerRow
                        key={worker.name}
                        worker={worker}
                        busy={busy === worker.name}
                        error={errors[worker.name] ?? null}
                        onRestart={() => requestRestart(worker)}
                    />
                ))}
            </Stack>

            {confirming && (
                <ConfirmModal
                    open
                    onOpenChange={open => {
                        if (!open) setConfirming(null)
                    }}
                    title={`Restart ${confirming.name}?`}
                    message={RESTART_WARNINGS[confirming.name]}
                    confirmLabel="Restart worker"
                    confirmColor="danger"
                    onConfirm={() => void run(confirming)}
                />
            )}
        </div>
    )
}
