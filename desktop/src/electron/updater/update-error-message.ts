// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export const UPDATE_CHECK_ERROR_MESSAGE = 'Error checking for updates'
const UPDATE_DOWNLOAD_ERROR_MESSAGE = 'Error downloading update'
const UPDATE_INSTALL_ERROR_MESSAGE = 'Error installing update'

export type UpdateOperation = 'check' | 'download' | 'install'

export function userFacingUpdateError(operation: UpdateOperation): string {
    if (operation === 'download') return UPDATE_DOWNLOAD_ERROR_MESSAGE
    if (operation === 'install') return UPDATE_INSTALL_ERROR_MESSAGE
    return UPDATE_CHECK_ERROR_MESSAGE
}
