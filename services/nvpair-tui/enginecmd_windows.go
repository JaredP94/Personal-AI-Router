// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
)

func executeEngine(path string, args, env []string) (int, error) {
	// Both processes share the console, so Windows delivers Ctrl-C directly
	// to the engine too. Keep this waiting parent alive; do not kill the child
	// or send it a second interrupt. In particular, do not create a new group.
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)

	cmd := exec.Command(path, args...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return 2, err
	}
	return 0, nil
}
