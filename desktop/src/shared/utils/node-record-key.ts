// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/** Broker-owned provenance. A host UUID in an unverified record is only a claim. */
export interface NodeRecordIdentity {
    hostUuid?: string
    peerRecordId?: string
    identityVerification?: string
}

/** Mirrors services/shared/noderec/noderec.go DirectoryNode.RecordKey exactly. */
export function nodeRecordKey(record: NodeRecordIdentity): string {
    if (record.identityVerification === 'local' || record.identityVerification === 'verified') {
        const hostUuid = record.hostUuid ?? ''
        return hostUuid.startsWith('preview:') || hostUuid.startsWith('manual:') ? '' : hostUuid
    }
    const peerRecordId = record.peerRecordId ?? ''
    return peerRecordId.startsWith('preview:') || peerRecordId.startsWith('manual:')
        ? peerRecordId
        : ''
}

/** Peer-only presentation/actions; this renders provenance, it never establishes trust. */
export function isPairPeer(record: NodeRecordIdentity): boolean {
    return (
        nodeRecordKey(record) !== '' &&
        (record.identityVerification === 'local' || record.identityVerification === 'verified')
    )
}

export function nodeVerificationLabel(record: NodeRecordIdentity): string {
    if (isPairPeer(record)) return ''
    return record.identityVerification === 'manual' || nodeRecordKey(record).startsWith('manual:')
        ? 'Engine only — added by address'
        : 'Not in your cluster'
}
