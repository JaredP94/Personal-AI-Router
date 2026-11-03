// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useState } from 'react'
import {
    Button,
    Divider,
    Flex,
    FormField,
    ModalContent,
    ModalDialog,
    ModalRoot,
    Stack,
    Text,
    TextInput
} from '@nvidia/foundations-react-core'
import { DialogHeader } from './DialogHeader'
import { InvitePairingPanel } from './InvitePairingPanel'
import {
    canPairByAddress,
    nodeRecordKey,
    nodeVerificationLabel
} from '@/shared/utils/node-record-key'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'
import { useBlurOnOpen } from '@/ui/hooks/useBlurOnOpen'
import { useInvitePairing } from '@/ui/hooks/useInvitePairing'
import { useInvitablePeers } from '@/ui/hooks/useInvitablePeers'
import type { ManualNodeKind } from '@/shared/types/manual-nodes'
import getErrorString from '@/shared/utils/get-error-string'

interface AddNodeModalProps {
    open: boolean
    onOpenChange: (open: boolean) => void
}

export function AddNodeModal({ open, onOpenChange }: AddNodeModalProps) {
    useBlurOnOpen(open)
    const [kind, setKind] = useState<ManualNodeKind>('pair')
    const [manualAddress, setManualAddress] = useState('')
    const [error, setError] = useState<string | null>(null)
    const [submitting, setSubmitting] = useState(false)
    const pairing = useInvitePairing()
    const nodesThatCanBeAdded = useInvitablePeers()

    const handleOpenChange = useCallback(
        (next: boolean) => {
            setManualAddress('')
            setKind('pair')
            setError(null)
            setSubmitting(false)
            pairing.reset()
            onOpenChange(next)
        },
        [onOpenChange, pairing]
    )

    const handleAddManualNode = useCallback(async () => {
        const address = manualAddress.trim()
        if (!address) return
        setError(null)
        setSubmitting(true)
        try {
            if (kind === 'pair') {
                // Save reachability before pairing; failed pairing keeps an unverified entry.
                await window.pairApi.manualNodes.add({ address, kind: 'pair' })
                void pairing.start(address)
            } else {
                await window.pairApi.manualNodes.add({ address, kind: 'engine' })
                handleOpenChange(false)
            }
        } catch (err) {
            setError(getErrorString(err))
        } finally {
            setSubmitting(false)
        }
    }, [manualAddress, kind, pairing, handleOpenChange])

    const showPairing = pairing.invite !== null || pairing.error !== null
    const inviteInFlight = pairing.submitting || pairing.invite?.state === 'pending'

    return (
        <ModalRoot open={open} onOpenChange={handleOpenChange} hideCloseButton>
            <ModalDialog>
                <ModalContent className="no-drag-elements max-content-modal">
                    <DialogHeader onClose={() => handleOpenChange(false)}>
                        <Flex align="center" gap="2">
                            <span>Add node</span>
                            {(pairing.submitting || submitting) && (
                                <span className="spinner-element" role="status" aria-label="" />
                            )}
                        </Flex>
                    </DialogHeader>
                    <Stack gap="4" className="pt-2">
                        {showPairing ? (
                            <InvitePairingPanel
                                invite={pairing.invite}
                                error={pairing.error}
                                onReset={pairing.reset}
                                onCancel={() => void pairing.cancel()}
                                onDone={() => handleOpenChange(false)}
                            />
                        ) : (
                            <>
                                {error && <InlineErrorBanner severity="error" message={error} />}

                                <Stack gap="2">
                                    <Text kind="body/semibold/sm">Node type</Text>
                                    <Flex gap="2">
                                        <Button
                                            kind={kind === 'pair' ? 'primary' : 'secondary'}
                                            color={kind === 'pair' ? 'brand' : 'neutral'}
                                            size="small"
                                            onClick={() => {
                                                setKind('pair')
                                                setError(null)
                                            }}
                                            disabled={inviteInFlight || submitting}
                                        >
                                            PAIR machine
                                        </Button>
                                        <Button
                                            kind={kind === 'engine' ? 'primary' : 'secondary'}
                                            color={kind === 'engine' ? 'brand' : 'neutral'}
                                            size="small"
                                            onClick={() => {
                                                setKind('engine')
                                                setError(null)
                                            }}
                                            disabled={inviteInFlight || submitting}
                                        >
                                            Engine only
                                        </Button>
                                    </Flex>
                                    {kind === 'engine' && (
                                        <Text kind="body/regular/sm" className="text-subtle-color">
                                            Manual bypass: PAIR trusts no PAIR claims from this
                                            engine. A PAIR machine’s engine is reachable only if its
                                            owner enabled LAN engine binding.
                                        </Text>
                                    )}
                                </Stack>

                                <Flex align="end" gap="2">
                                    <FormField
                                        slotLabel="Address (hostname or IP)"
                                        className="flex-1"
                                    >
                                        <TextInput
                                            value={manualAddress}
                                            onValueChange={setManualAddress}
                                            placeholder="192.168.1.100 or host.local"
                                            onKeyDown={event => {
                                                if (event.key === 'Enter') {
                                                    void handleAddManualNode()
                                                }
                                            }}
                                            disabled={inviteInFlight || submitting}
                                        />
                                    </FormField>
                                    <Button
                                        kind="primary"
                                        color="brand"
                                        onClick={handleAddManualNode}
                                        disabled={
                                            !manualAddress.trim() || inviteInFlight || submitting
                                        }
                                    >
                                        {kind === 'pair' ? 'Invite' : 'Add engine'}
                                    </Button>
                                </Flex>

                                {nodesThatCanBeAdded.length > 0 && (
                                    <Stack gap="4" className="mt-1">
                                        <Divider />

                                        <Stack gap="1">
                                            {nodesThatCanBeAdded.map(node => (
                                                <Flex
                                                    key={nodeRecordKey(node)}
                                                    align="center"
                                                    justify="between"
                                                    gap="2"
                                                    className="py-1"
                                                >
                                                    <Stack gap="0">
                                                        <Text
                                                            kind="body/semibold/sm"
                                                            className="uppercase"
                                                        >
                                                            {node.name || node.ipAddress}
                                                        </Text>
                                                        <Text
                                                            kind="body/regular/sm"
                                                            className="text-subtle-color"
                                                        >
                                                            {nodeVerificationLabel(node)}
                                                        </Text>
                                                        <Text
                                                            kind="body/regular/sm"
                                                            className="text-subtle-color"
                                                        >
                                                            {node.ipAddress}
                                                        </Text>
                                                    </Stack>
                                                    {canPairByAddress(node) && (
                                                        <Button
                                                            kind="primary"
                                                            color="brand"
                                                            size="small"
                                                            onClick={() =>
                                                                void pairing.start(node.ipAddress)
                                                            }
                                                            // Public cluster claims do not establish
                                                            // identity. Pair by address; the PIN
                                                            // exchange reports any real rejection.
                                                            disabled={
                                                                inviteInFlight || !node.ipAddress
                                                            }
                                                        >
                                                            Pair
                                                        </Button>
                                                    )}
                                                </Flex>
                                            ))}
                                        </Stack>
                                    </Stack>
                                )}
                            </>
                        )}
                    </Stack>
                </ModalContent>
            </ModalDialog>
        </ModalRoot>
    )
}
