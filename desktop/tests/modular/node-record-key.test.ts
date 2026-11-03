// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import fixture from '@/shared/fixtures/node-record-keys.json'
import { nodeRecordKey } from '@/shared/utils/node-record-key'

describe('Go DirectoryNode.RecordKey parity', () => {
    it.each(fixture.cases)('$name', record => {
        expect(nodeRecordKey(record)).toBe(record.want)
    })
})
