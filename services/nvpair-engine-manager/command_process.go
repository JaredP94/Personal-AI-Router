// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"bytes"
	"errors"
	"os/exec"
)

func ownedCombinedOutput(cmd *exec.Cmd, detachedControl bool) ([]byte, error) {
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := runOwnedCommandMode(cmd, detachedControl)
	return output.Bytes(), err
}

func ownedOutput(cmd *exec.Cmd) ([]byte, error) {
	var output, diagnostic bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &diagnostic
	err := runOwnedCommand(cmd)
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		exit.Stderr = diagnostic.Bytes()
	}
	return output.Bytes(), err
}
