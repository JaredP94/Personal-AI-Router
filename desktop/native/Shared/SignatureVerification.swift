// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import Foundation
import Security

func verifySignature(path: String, requirement requirementString: String) -> Bool {
    let url = URL(fileURLWithPath: path)
    var staticCode: SecStaticCode?
    guard SecStaticCodeCreateWithPath(url as CFURL, [], &staticCode) == errSecSuccess,
        let code = staticCode
    else {
        return false
    }
    var requirement: SecRequirement?
    guard SecRequirementCreateWithString(requirementString as CFString, [], &requirement)
        == errSecSuccess, let req = requirement
    else {
        return false
    }
    return SecStaticCodeCheckValidity(code, [], req) == errSecSuccess
}
