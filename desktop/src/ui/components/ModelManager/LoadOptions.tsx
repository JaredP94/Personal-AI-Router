// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useRef, useState } from 'react'
import { Button, Flex, FormField, Stack, Text, TextArea } from '@nvidia/foundations-react-core'
import type { EngineType } from '@/shared/types/engines'
import type { NodeItem } from '@/shared/types/nodes'
import type {
    LoadProfileTarget,
    LoadProfileSnapshot,
    LoadProfileRequest
} from '@/shared/types/load-profile'
import { isPairPeer, nodeRecordKey } from '@/shared/utils/node-record-key'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useNodesStore } from '@/ui/stores/nodes.store'

function profileTargetAllowed(
    nodeId: string,
    selfId: string | null,
    node: NodeItem | undefined
): boolean {
    if (!nodeId || nodeId.startsWith('preview:') || nodeId.startsWith('manual:')) return false
    if (selfId === nodeId) return true
    return (
        !!node &&
        node.identityVerification === 'verified' &&
        isPairPeer(node) &&
        nodeRecordKey(node) === nodeId
    )
}

/** A lazy editor in each downloaded LM Studio model row, never a second store. */
export function LoadOptions({
    nodeId,
    model,
    engineType,
    disabled = false,
    loaded = false
}: {
    nodeId: string
    model: string
    engineType: EngineType
    disabled?: boolean
    loaded?: boolean
}) {
    const selfId = useConnectionStore(state => state.selfId)
    const node = useNodesStore(state => state.nodes.get(nodeId))
    // Subscribe to provenance changes as well as self identity. The helper's
    // result only controls presentation; Go independently enforces peer trust.
    if (engineType !== 'lm-studio' || !profileTargetAllowed(nodeId, selfId, node)) return null
    return (
        <LoadOptionsRow
            key={JSON.stringify([nodeId, model])}
            nodeId={nodeId}
            model={model}
            disabled={disabled}
            loaded={loaded}
        />
    )
}

function LoadOptionsRow({
    nodeId,
    model,
    disabled,
    loaded
}: {
    nodeId: string
    model: string
    disabled: boolean
    loaded: boolean
}) {
    const [expanded, setExpanded] = useState(false)
    const [snapshot, setSnapshot] = useState<LoadProfileSnapshot>()
    const [residencyError, setResidencyError] = useState('')
    const connected = useConnectionStore(state => state.connected)
    const offline = useNodesStore(state => state.nodes.get(nodeId)?.status === 'offline')
    const generation = useRef(0)
    const acceptSnapshot = useCallback((result: LoadProfileSnapshot): void => {
        generation.current++
        setSnapshot(result)
        setResidencyError('')
    }, [])
    useEffect(() => {
        const id = ++generation.current
        setSnapshot(undefined)
        setResidencyError('')
        if (loaded && connected && !offline && !disabled) {
            void window.pairApi.engines
                .getLoadProfile({ nodeId, model, engine: 'lmstudio' })
                .then(result => {
                    if (generation.current === id) setSnapshot(result)
                })
                .catch(() => {
                    if (generation.current === id)
                        setResidencyError(
                            'Loaded model options could not be checked. Reload saved options to retry.'
                        )
                })
        }
        return invalidate
        function invalidate(): void {
            generation.current++
        }
    }, [nodeId, model, loaded, connected, offline, disabled])
    const residencyMessage =
        snapshot?.loadState === 'reload-required' || snapshot?.loadState === 'unknown'
            ? snapshot.reason
            : residencyError
    return (
        <details
            className="pair-accordion translucent-bg-accordion ml-4"
            onToggle={event => setExpanded(event.currentTarget.open)}
        >
            <summary className="pair-accordion-summary">
                <Text kind="body/regular/sm">Load options</Text>
                {residencyMessage && (
                    <Text kind="body/regular/sm" role="status">
                        {residencyMessage}
                    </Text>
                )}
            </summary>
            {expanded && (
                <LoadOptionsEditor
                    key={JSON.stringify([nodeId, model])}
                    target={{ nodeId, engine: 'lmstudio', model }}
                    disabled={disabled}
                    onSnapshot={acceptSnapshot}
                />
            )}
        </details>
    )
}

const changedMessage =
    'Load options changed elsewhere. Saved options were reloaded; review them before applying.'

function LoadOptionsEditor({
    target,
    disabled,
    onSnapshot
}: {
    target: LoadProfileTarget
    disabled: boolean
    onSnapshot: (snapshot: LoadProfileSnapshot) => void
}) {
    const { nodeId, model } = target
    const connected = useConnectionStore(state => state.connected)
    const offline = useNodesStore(state => state.nodes.get(nodeId)?.status === 'offline')
    const [snapshot, setSnapshot] = useState<LoadProfileSnapshot>()
    const [draft, setDraft] = useState('')
    const [loading, setLoading] = useState(true)
    const [checking, setChecking] = useState(false)
    const [applying, setApplying] = useState(false)
    const [needsRefresh, setNeedsRefresh] = useState(false)
    const [message, setMessage] = useState('')
    const generation = useRef(0)
    const disabledNow = useRef(disabled)
    disabledNow.current = disabled
    const supported = snapshot?.editable && snapshot.format === 'pair-arguments-v1'
    const locked = !connected || offline || disabled || !supported || applying || needsRefresh
    const dirty = !!snapshot && draft !== snapshot.loadText

    const available = useCallback((): boolean => {
        return (
            !disabledNow.current &&
            useConnectionStore.getState().connected &&
            useNodesStore.getState().nodes.get(nodeId)?.status !== 'offline' &&
            profileTargetAllowed(
                nodeId,
                useConnectionStore.getState().selfId,
                useNodesStore.getState().nodes.get(nodeId)
            )
        )
    }, [nodeId])
    const reload = useCallback(
        async (conflict = false): Promise<void> => {
            const id = ++generation.current
            setLoading(true)
            setChecking(false)
            setMessage('')
            try {
                const result = await window.pairApi.engines.getLoadProfile({
                    nodeId,
                    engine: 'lmstudio',
                    model
                })
                if (generation.current !== id || !available()) return
                setSnapshot(result)
                onSnapshot(result)
                setDraft(result.loadText)
                setNeedsRefresh(false)
                setMessage(conflict ? changedMessage : '')
            } catch (cause) {
                if (generation.current !== id) return
                setNeedsRefresh(true)
                setMessage(
                    cause instanceof Error
                        ? cause.message
                        : 'Load options unavailable. Reconnect or update this device, then reload.'
                )
            } finally {
                if (generation.current === id) setLoading(false)
            }
        },
        [nodeId, model, available, onSnapshot]
    )
    useEffect(() => {
        generation.current++
        setApplying(false)
        setChecking(false)
        if (connected && !offline && !disabled) void reload()
        return invalidate
        function invalidate(): void {
            generation.current++
        }
    }, [reload, connected, offline, disabled])

    function change(value: string): void {
        generation.current++
        setLoading(false)
        setChecking(false)
        setMessage('')
        setDraft(value)
    }
    async function apply(): Promise<void> {
        if (!snapshot || locked || checking || !dirty || !available()) return
        const id = ++generation.current
        const request: LoadProfileRequest = {
            ...target,
            expectedRevision: snapshot.revision,
            format: snapshot.format,
            loadText: draft
        }
        setChecking(true)
        setMessage('')
        try {
            const preview = await window.pairApi.engines.previewLoadProfile(request)
            if (generation.current !== id || !available()) return
            if (preview.revision !== snapshot.revision) {
                await reload(true)
                return
            }
            if (preview.format !== 'pair-arguments-v1') {
                setMessage('This load options format is not supported.')
                return
            }
            const error = Object.values(preview.errors).find(Boolean)
            if (error) {
                setMessage(error)
                return
            }
            setChecking(false)
            setApplying(true)
            const result = await window.pairApi.engines.applyLoadProfile({
                ...request,
                loadText: preview.loadText,
                requestId: crypto.randomUUID()
            })
            if (generation.current !== id || !available()) return
            setSnapshot(result)
            onSnapshot(result)
            setDraft(result.loadText)
            setMessage('Saved for the next load.')
        } catch (cause) {
            if (generation.current !== id || !available()) return
            // A lost response can follow an accepted atomic write. Never retry
            // against the old revision; require a fresh authoritative read.
            setNeedsRefresh(true)
            setMessage(
                cause instanceof Error
                    ? cause.message
                    : 'Load options could not be saved. Reload before retrying.'
            )
        } finally {
            if (generation.current === id) {
                setChecking(false)
                setApplying(false)
            }
        }
    }

    return (
        <Stack gap="3" className="p-3">
            {loading && <Text kind="body/regular/sm">Loading load options…</Text>}
            <Text kind="body/regular/sm">
                Saved options apply on the next load on this device. The current model will not
                restart.
            </Text>
            {snapshot && !supported && (
                <Text kind="body/regular/sm">
                    {snapshot.reason ||
                        'Editing is not supported by this device or argument format.'}
                </Text>
            )}
            <FormField slotLabel="LM Studio load arguments">
                <TextArea
                    size="small"
                    aria-label="LM Studio load options"
                    rows={3}
                    maxLength={16384}
                    className="font-mono"
                    value={draft}
                    disabled={locked}
                    placeholder="--gpu max --context-length 200000 --parallel 2"
                    onValueChange={change}
                />
            </FormField>
            <Text kind="body/regular/sm">
                Only supported load-tuning options are accepted, with explicit values (including
                true/false for booleans). Leave empty for engine defaults. Model identity, server,
                file and execution controls are unavailable here.
            </Text>
            {message && (
                <Text
                    kind="body/regular/sm"
                    role={message === 'Saved for the next load.' ? 'status' : 'alert'}
                >
                    {message}
                </Text>
            )}
            <Flex gap="2" justify="end">
                <Button
                    kind="secondary"
                    size="small"
                    disabled={applying || !connected || offline || disabled}
                    onClick={() => void reload()}
                >
                    {dirty || needsRefresh ? 'Reload saved options' : 'Refresh'}
                </Button>
                <Button
                    kind="primary"
                    color="brand"
                    size="small"
                    disabled={locked || checking || !dirty}
                    aria-busy={checking || applying}
                    onClick={() => void apply()}
                >
                    {applying ? 'Applying…' : checking ? 'Validating…' : 'Apply'}
                </Button>
            </Flex>
        </Stack>
    )
}
