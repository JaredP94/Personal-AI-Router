// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/** Render upstream text without interpreting it as notice Markdown. */
export function renderLicenseText(text: string): string {
    const longestRun = (text.match(/`+/g) ?? []).reduce(
        (longest, run) => Math.max(longest, run.length),
        0
    )
    const fence = '`'.repeat(Math.max(3, longestRun + 1))
    return [`${fence}text`, text, fence].join('\n')
}
