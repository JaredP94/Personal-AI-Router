// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !darwin

package main

// hostOSMajorVersion is 0 off macOS: fetch_variants keyed on a macOS release
// are macOS-only, so only a variant with no minimum matches.
func hostOSMajorVersion() int { return 0 }
