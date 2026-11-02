// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

// fileIdentity is unavailable on Windows, where no bundled manifest uses
// install.creates; a claim that names one is therefore never confirmed.
func fileIdentity(string) string { return "" }
