// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Button } from '@nvidia/foundations-react-core'

export function ModelDownloadControls({
    canCancel,
    opId,
    status,
    onCancel
}: {
    canCancel?: boolean
    opId?: string
    status?: string
    onCancel?: () => void
}) {
    if (!canCancel || !opId || !onCancel || status === 'cancelled') return null
    return (
        <Button
            size="small"
            kind="tertiary"
            onClick={onCancel}
            title={status === 'canceling' ? 'Request cancellation again' : 'Cancel download'}
        >
            Cancel
        </Button>
    )
}
