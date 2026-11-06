// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import "os/exec"

func ownedCombinedOutput(cmd *exec.Cmd, _ bool) ([]byte, error) { return cmd.CombinedOutput() }
func ownedOutput(cmd *exec.Cmd) ([]byte, error)                 { return cmd.Output() }
func runOwnedCommandMode(cmd *exec.Cmd, _ bool) error           { return cmd.Run() }
func runOwnedCommand(cmd *exec.Cmd) error                       { return cmd.Run() }
