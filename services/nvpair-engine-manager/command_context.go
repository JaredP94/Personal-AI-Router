// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "context"

type controlCommandKey struct{}

// Only command-mode start/stop declares an independently managed daemon tree.
// Install/uninstall helpers and action CLIs retain normal tree ownership.
func withControlCommand(ctx context.Context) context.Context {
	return context.WithValue(ctx, controlCommandKey{}, true)
}
func isControlCommand(ctx context.Context) bool {
	control, _ := ctx.Value(controlCommandKey{}).(bool)
	return control
}
