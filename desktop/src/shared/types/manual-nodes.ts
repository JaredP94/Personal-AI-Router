// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export type ManualNodeKind = 'pair' | 'engine'

export interface ManualNodeAddRequest {
    address: string
    name?: string
    kind: ManualNodeKind
    tls_port?: number
    mtls?: boolean
}

export interface ManualNodeRemoveRequest {
    id: string
}

export interface ManualNodeStatus {
    id: string
    address: string
    name?: string
    kind: ManualNodeKind
    identityVerification?: string
    peerRecordId?: string
    hostUuid?: string
    clusterUuid?: string
    ollama_up?: boolean
    ollama_port?: number
    ollama_models?: string[]
    lmstudio_up?: boolean
    lmstudio_port?: number
    lmstudio_models?: string[]
    omlx_up?: boolean
    omlx_port?: number
    omlx_models?: string[]
    node_info_up?: boolean
    node_info_port?: number
    tls_enabled?: boolean
    mtls_required?: boolean
    models?: string[]
}
