// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows && !linux && !darwin

package main

// procImage has no implementation on this platform. An empty image makes the
// caller's ownership check fail closed, so a stop of an engine PAIR lost the
// handle to is declined.
func procImage(int) string { return "" }
