// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useRef, useState } from 'react'
import {
    Button,
    Flex,
    FormField,
    Stack,
    Text,
    TextArea,
    TextInput
} from '@nvidia/foundations-react-core'
import type { EngineType } from '@/shared/types/engines'
import type {
    EngineSettingsConfig,
    EngineSettingsRequest,
    EngineSettingsSnapshot,
    EngineSettingsTarget
} from '@/shared/types/engine-settings'
import { isPairPeer } from '@/shared/utils/node-record-key'
import {
    engineSettingsKey,
    useEngineSettingsStore,
    watchEngineSettings
} from '@/ui/stores/engine-settings.store'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useNodesStore } from '@/ui/stores/nodes.store'
import { useLlamaInstalledModels } from '@/ui/hooks/useLlamaInstalledModels'
import { selectLlamaModel, selectedLlamaModel } from '@/ui/utils/llamacpp-models'
import { ConfirmModal } from '@/ui/components/ConfirmModal'

interface Draft {
    serverPort: string
    proxyPort: string
    launchText: string
}
const toDraft = (settings: EngineSettingsConfig): Draft => ({
    ...settings,
    serverPort: String(settings.serverPort),
    proxyPort: String(settings.proxyPort)
})
const sameDraft = (a: Draft, b: Draft): boolean =>
    a.serverPort === b.serverPort && a.proxyPort === b.proxyPort && a.launchText === b.launchText
const changedMessage =
    'Settings changed elsewhere. Saved settings have been reloaded; review them before applying.'

export function EngineSettingsSection({
    nodeId,
    engineType,
    disabled
}: {
    nodeId: string
    engineType: EngineType
    disabled: boolean
}) {
    const local = useConnectionStore(state => state.selfId === nodeId)
    const peer = useNodesStore(state => {
        const node = state.nodes.get(nodeId)
        return !!node && isPairPeer(node)
    })
    if (nodeId.startsWith('preview:') || nodeId.startsWith('manual:') || (!local && !peer))
        return null
    const engine = engineType === 'lm-studio' ? 'lmstudio' : engineType
    return (
        <SettingsEditor
            key={`${nodeId}:${engine}`}
            target={{ nodeId, engine }}
            disabled={disabled}
        />
    )
}

function EffectivePortNote({ configured, effective }: { configured?: number; effective?: number }) {
    if (!configured || !effective || effective === configured) return null
    return (
        <Text kind="body/regular/sm">
            {`Running on port ${effective} because ${configured} was busy. PAIR will try ${configured} again next start.`}
        </Text>
    )
}

function SettingsEditor({ target, disabled }: { target: EngineSettingsTarget; disabled: boolean }) {
    const { nodeId, engine } = target
    const key = engineSettingsKey(target)
    const entry = useEngineSettingsStore(state => state.entries[key])
    const snapshot = entry?.snapshot
    const connected = useConnectionStore(state => state.connected)
    const offline = useNodesStore(state => state.nodes.get(nodeId)?.status === 'offline')
    const [base, setBase] = useState<EngineSettingsSnapshot>()
    const [draft, setDraft] = useState<Draft>({ serverPort: '', proxyPort: '', launchText: '' })
    const [message, setMessage] = useState('')
    const [errors, setErrors] = useState<Record<string, string>>({})
    const [confirmation, setConfirmation] = useState<EngineSettingsRequest>()
    const [checking, setChecking] = useState(false)
    const generation = useRef(0)
    const submitted = useRef('')
    const dirty = !!base && !sameDraft(draft, toDraft(base.settings))
    const applying = !!entry?.pending || snapshot?.phase === 'applying'
    const readOnly =
        !connected ||
        offline ||
        !!entry?.unavailable ||
        !snapshot?.editable ||
        snapshot.format !== 'pair-arguments-v1'
    const locked = readOnly || disabled || applying
    const isOmlx = engine === 'omlx'
    const local = useConnectionStore(state => state.selfId === nodeId)
    const installed = useLlamaInstalledModels(nodeId, engine === 'llamacpp' && local)
    const selected = selectedLlamaModel(draft.launchText)

    useEffect(() => watchEngineSettings(), [])
    useEffect(() => {
        generation.current++
        setChecking(false)
        setConfirmation(undefined)
        if (connected && !offline) void useEngineSettingsStore.getState().reload({ nodeId, engine })
        else useEngineSettingsStore.getState().disconnect(nodeId)
        return invalidate
        function invalidate(): void {
            generation.current += 1
        }
    }, [nodeId, engine, connected, offline])

    useEffect(() => {
        if (entry?.unavailable || disabled) {
            generation.current++
            setChecking(false)
            setConfirmation(undefined)
        }
    }, [entry?.unavailable, disabled])

    useEffect(() => {
        if (!snapshot) return
        const ownResult =
            !!submitted.current &&
            submitted.current === snapshot.requestId &&
            snapshot.phase !== 'applying'
        if (!base || !dirty || ownResult) {
            setBase(snapshot)
            setDraft(toDraft(snapshot.settings))
            if (ownResult) submitted.current = ''
        }
        // A confirmation is only valid for the revision that was previewed.
        if (base && (base.revision !== snapshot.revision || base.epoch !== snapshot.epoch))
            setConfirmation(undefined)
    }, [snapshot, base, dirty])

    function change(field: keyof Draft, value: string): void {
        generation.current++
        setChecking(false)
        setConfirmation(undefined)
        setMessage('')
        setErrors({})
        setDraft(current => ({ ...current, [field]: value }))
    }
    function stale(): boolean {
        const current = useEngineSettingsStore.getState().entries[key]?.snapshot
        return (
            !!base &&
            !!current &&
            (base.revision !== current.revision || base.epoch !== current.epoch)
        )
    }
    async function reload(conflict = false): Promise<void> {
        const id = ++generation.current
        setChecking(false)
        setConfirmation(undefined)
        setErrors({})
        await useEngineSettingsStore.getState().reload(target)
        if (generation.current !== id) return
        const current = useEngineSettingsStore.getState().entries[key]?.snapshot
        if (current) {
            setBase(current)
            setDraft(toDraft(current.settings))
        }
        setMessage(conflict ? changedMessage : '')
    }
    function canSubmit(): boolean {
        const current = useEngineSettingsStore.getState().entries[key]
        const connection = useConnectionStore.getState()
        const node = useNodesStore.getState().nodes.get(nodeId)
        return (
            connection.connected &&
            !current?.unavailable &&
            !!current?.snapshot?.editable &&
            current.snapshot.format === 'pair-arguments-v1' &&
            !current.pending &&
            current.snapshot.phase !== 'applying' &&
            node?.status !== 'offline' &&
            (connection.selfId === nodeId || (!!node && isPairPeer(node)))
        )
    }
    async function apply(request: EngineSettingsRequest): Promise<void> {
        if (locked || !canSubmit()) return
        if (stale()) {
            await reload(true)
            return
        }
        submitted.current = request.requestId ?? ''
        setMessage('')
        try {
            await useEngineSettingsStore.getState().apply(request)
        } catch (cause) {
            submitted.current = ''
            if (stale()) await reload(true)
            else
                setMessage(
                    cause instanceof Error ? cause.message : 'Could not apply engine settings.'
                )
        }
    }
    async function preview(): Promise<void> {
        if (!base || locked || checking || confirmation) return
        if (stale()) {
            await reload(true)
            return
        }
        const id = ++generation.current
        const serverPort = Number(draft.serverPort)
        const proxyPort = Number(draft.proxyPort)
        if (
            ![serverPort, proxyPort].every(
                port => Number.isInteger(port) && port >= 1 && port <= 65535
            )
        ) {
            setMessage('Enter ports from 1 to 65535.')
            return
        }
        const request: EngineSettingsRequest = {
            ...target,
            expectedRevision: base.revision,
            settings: { serverPort, proxyPort, launchText: isOmlx ? '' : draft.launchText }
        }
        setChecking(true)
        setMessage('')
        setErrors({})
        try {
            const result = await window.pairApi.engines.previewSettings(request)
            if (generation.current !== id || !canSubmit()) return
            if (stale() || result.revision !== base.revision) {
                await reload(true)
                return
            }
            if (Object.keys(result.errors).length) {
                setErrors(result.errors)
                return
            }
            const accepted = {
                ...request,
                settings: result.settings,
                requestId: crypto.randomUUID()
            }
            if (result.restart) setConfirmation(accepted)
            else void apply(accepted)
        } catch (cause) {
            if (generation.current !== id) return
            // Read back on a rejected preview too: a revision can change before
            // its push arrives. The authority's revision decides the outcome.
            await useEngineSettingsStore.getState().reload(target)
            if (generation.current !== id) return
            if (stale()) await reload(true)
            else
                setMessage(
                    cause instanceof Error ? cause.message : 'Could not preview engine settings.'
                )
        } finally {
            if (generation.current === id) setChecking(false)
        }
    }

    const failure =
        message ||
        (!applying && !Object.keys(errors).length && snapshot?.phase === 'failed'
            ? snapshot.error
            : '')
    return (
        <details className="pair-accordion translucent-bg-accordion">
            <summary className="pair-accordion-summary">
                <Text kind="body/semibold/sm">Settings</Text>
            </summary>
            <Stack gap="3" className="p-3">
                {!snapshot && !entry?.unavailable && (
                    <Text kind="body/regular/sm">Loading settings…</Text>
                )}
                {(entry?.unavailable || (snapshot && readOnly)) && (
                    <Text kind="body/regular/sm">
                        {entry?.unavailable ||
                            snapshot?.reason ||
                            'Editing is not supported by this device or settings format.'}
                    </Text>
                )}
                <Flex gap="3">
                    <FormField slotLabel="Server port">
                        <TextInput
                            size="small"
                            aria-label="Server port"
                            value={draft.serverPort}
                            onValueChange={value => change('serverPort', value)}
                            disabled={locked}
                            readOnly={isOmlx}
                            inputMode="numeric"
                            aria-invalid={!!errors.serverPort}
                        />
                        <EffectivePortNote
                            configured={snapshot?.settings.serverPort}
                            effective={
                                entry?.unavailable || offline || !connected
                                    ? 0
                                    : snapshot?.effectiveServerPort
                            }
                        />
                    </FormField>
                    {
                        <FormField slotLabel="Proxy port">
                            <TextInput
                                size="small"
                                aria-label="Proxy port"
                                value={draft.proxyPort}
                                onValueChange={value => change('proxyPort', value)}
                                disabled={locked}
                                inputMode="numeric"
                                aria-invalid={!!errors.proxyPort}
                            />
                            <EffectivePortNote
                                configured={snapshot?.settings.proxyPort}
                                effective={
                                    entry?.unavailable || offline || !connected
                                        ? 0
                                        : snapshot?.effectiveProxyPort
                                }
                            />
                        </FormField>
                    }
                </Flex>
                {engine === 'llamacpp' && (
                    <FormField slotLabel="Startup model">
                        <select
                            aria-label="Startup model"
                            disabled={locked || (!local && !!selected)}
                            value={
                                installed.models.find(model => model.id === selected)?.file ??
                                selected
                            }
                            onChange={event =>
                                change(
                                    'launchText',
                                    selectLlamaModel(draft.launchText, event.target.value)
                                )
                            }
                        >
                            <option value="">No model (router mode)</option>
                            {selected &&
                                !installed.models.some(
                                    model => model.file === selected || model.id === selected
                                ) && (
                                    <option value={selected}>
                                        {selected.startsWith('/dev/fd/')
                                            ? 'Unavailable model'
                                            : selected}
                                    </option>
                                )}
                            {installed.models.map(model => (
                                <option key={model.id} value={model.file}>
                                    {model.id}
                                </option>
                            ))}
                        </select>
                        {installed.error && <Text kind="body/regular/sm">{installed.error}</Text>}
                        {!local && (
                            <Text kind="body/regular/sm">
                                Paired-device cache listing is not available yet. Engine arguments
                                can select a known installed catalog ID.
                            </Text>
                        )}
                    </FormField>
                )}
                {isOmlx ? (
                    <Text kind="body/regular/sm">
                        {"oMLX's server settings are managed in the oMLX app"}
                    </Text>
                ) : (
                    <FormField slotLabel="Engine arguments and environment assignments">
                        <TextArea
                            size="small"
                            aria-label="Engine arguments"
                            placeholder="Optional NAME=value assignments followed by arguments"
                            value={draft.launchText}
                            onValueChange={value => change('launchText', value)}
                            rows={4}
                            maxLength={16384}
                            disabled={locked}
                            className="font-mono"
                            aria-invalid={!!errors.launchText}
                        />
                    </FormField>
                )}
                {Object.entries(errors).map(([field, error]) => (
                    <Text key={field} kind="body/regular/sm" role="alert">
                        {field === 'serverPort'
                            ? 'Server port'
                            : field === 'proxyPort'
                              ? 'Proxy port'
                              : 'Engine arguments'}
                        : {error}
                    </Text>
                ))}
                {failure && (
                    <Text kind="body/regular/sm" role="alert">
                        {failure}
                    </Text>
                )}
                <Flex gap="2" justify="end">
                    <Button
                        kind="secondary"
                        size="small"
                        disabled={applying || !connected || offline}
                        onClick={() => void reload()}
                    >
                        {dirty ? 'Reload saved settings' : 'Refresh'}
                    </Button>
                    <Button
                        kind="primary"
                        color="brand"
                        size="small"
                        disabled={locked || checking || (!dirty && snapshot?.phase !== 'failed')}
                        aria-busy={applying || checking}
                        onClick={() => void preview()}
                    >
                        {applying ? 'Applying…' : checking ? 'Validating…' : 'Apply'}
                    </Button>
                </Flex>
            </Stack>
            <ConfirmModal
                open={!!confirmation}
                onOpenChange={open => {
                    if (!open) setConfirmation(undefined)
                }}
                title="Restart engine?"
                confirmLabel="Restart"
                message="Applying these settings will restart the engine. Continue?"
                onConfirm={() => {
                    if (confirmation) void apply(confirmation)
                }}
            />
        </details>
    )
}
