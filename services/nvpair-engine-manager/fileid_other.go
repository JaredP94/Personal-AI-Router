// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !darwin && !linux && !windows

package main

// Unsupported targets cannot prove a generation-stable install identity.
// Fail closed: a claim naming a created app never authorizes its removal.
func fileIdentity(string) string { return "" }
