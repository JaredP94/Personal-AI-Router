// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
)

type engineCLIResolution struct {
	Engine string            `json:"engine"`
	Path   string            `json:"path"`
	Env    map[string]string `json:"env"`
}

type engineCLIEntry struct {
	Engine      string `json:"engine"`
	DisplayName string `json:"display_name"`
	Installed   bool   `json:"installed"`
	Supported   bool   `json:"supported"`
}

// runEngineCommand is independent of TUI flags, logging, and broker ownership.
func runEngineCommand(args []string) int {
	code, err := engineCommand(args)
	if err != nil {
		// Resolver failures have one stderr reason line. Keep local failures to
		// the same shape, even if a filesystem name contains a newline.
		message := strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(err.Error())
		fmt.Fprintln(os.Stderr, "nvpair: "+message)
		return 2
	}
	return code
}

func engineCommand(args []string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 2, fmt.Errorf("locate own executable: %w", err)
	}
	// Match supervisor.go's installed sibling layout; never search PATH.
	managerName := "nvpair-engine-manager"
	if runtime.GOOS == "windows" {
		managerName += ".exe"
	}
	manager := filepath.Join(filepath.Dir(exe), managerName)
	resolverArgs := []string{"--list-engines"}
	if len(args) > 0 {
		resolverArgs = []string{"--resolve", args[0]}
	}
	cmd := exec.Command(manager, resolverArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if reason := strings.TrimSpace(stderr.String()); reason != "" {
			return 2, fmt.Errorf("%s", reason)
		}
		return 2, fmt.Errorf("run %s: %w", managerName, err)
	}
	if len(args) == 0 {
		var entries []engineCLIEntry
		if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
			return 2, fmt.Errorf("decode engine list: %w", err)
		}
		table := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(table, "ENGINE\tNAME\tINSTALLED\tSUPPORTED")
		for _, entry := range entries {
			fmt.Fprintf(table, "%s\t%s\t%t\t%t\n", entry.Engine, entry.DisplayName, entry.Installed, entry.Supported)
		}
		if err := table.Flush(); err != nil {
			return 2, fmt.Errorf("write engine list: %w", err)
		}
		_, err := fmt.Fprintln(os.Stdout, "\nnvpair engine <engine> [args...]")
		return 0, err
	}
	var resolved engineCLIResolution
	if err := json.Unmarshal(stdout.Bytes(), &resolved); err != nil {
		return 2, fmt.Errorf("decode engine executable: %w", err)
	}
	if resolved.Path == "" {
		return 2, fmt.Errorf("engine-manager returned an empty executable path")
	}
	// A relative manifest path is relative to the caller's working directory,
	// not a command name to look up on PATH (including on Windows).
	path, err := filepath.Abs(resolved.Path)
	if err != nil {
		return 2, fmt.Errorf("resolve executable path: %w", err)
	}
	return executeEngine(path, args[1:], engineCLIEnvironment(resolved.Env))
}

// syscall.Exec needs the entire environment with replacements, not just the
// resolver's additions. Do not mutate this process or the user's environment.
func engineCLIEnvironment(overlay map[string]string) []string {
	env := os.Environ()
	for key, value := range overlay {
		kept := env[:0]
		for _, entry := range env {
			name, _, _ := strings.Cut(entry, "=")
			matches := name == key
			if runtime.GOOS == "windows" {
				matches = strings.EqualFold(name, key)
			}
			if !matches {
				kept = append(kept, entry)
			}
		}
		env = append(kept, key+"="+value)
	}
	return env
}
