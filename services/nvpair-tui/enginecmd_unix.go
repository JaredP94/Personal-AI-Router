// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import "syscall"

// Exec preserves the terminal, process identity, exit status, and signal handling
// for the engine. No TUI signal handlers have been installed on this path.
func executeEngine(path string, args, env []string) (int, error) {
	return 2, syscall.Exec(path, append([]string{path}, args...), env)
}
