// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"time"
)

const (
	codeHomebrewMissing     = -32010
	codeUnsupportedPlatform = -32011
	codeToolInstallFailed   = -32012
)

// toolError carries a JSON-RPC code through local and remote paths.
type toolError struct {
	Code int
	Msg  string
}

func (e *toolError) Error() string { return e.Msg }

type toolSpec struct {
	Platforms []string
	Detect    []string
	Formula   string
}

// toolEnv is immutable after construction and injectable for tests. Tools are
// deliberately separate from engine manifests and the engine registry.
type toolEnv struct {
	InstallTimeout time.Duration
	Platform       string
	BrewPaths      []string
	Tools          map[string]toolSpec
}

func defaultToolEnv() toolEnv {
	return toolEnv{
		InstallTimeout: 15 * time.Minute,
		Platform:       runtime.GOOS + "/" + runtime.GOARCH,
		BrewPaths:      []string{"/opt/homebrew/bin/brew", "/usr/local/bin/brew"},
		Tools: map[string]toolSpec{
			"macmon": {
				Platforms: []string{"darwin/arm64"},
				Detect:    []string{"/opt/homebrew/bin/macmon", "/usr/local/bin/macmon"},
				Formula:   "macmon",
			},
		},
	}
}

// firstToolExecutable checks only the ordered, explicit candidates. Stat follows
// Homebrew's symlinks, but directories and non-executable files do not count.
func firstToolExecutable(paths []string) string {
	for _, path := range paths {
		info, err := os.Stat(path)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return path
		}
	}
	return ""
}

// InstallTool installs an allowlisted tool as the current user. It does not
// alter engine state, search PATH, or override the user's Homebrew environment.
func (e *Executor) InstallTool(ctx context.Context, name string) error {
	e.toolInstallMu.Lock()
	defer e.toolInstallMu.Unlock()
	spec, ok := e.tools.Tools[name]
	if !ok {
		return fmt.Errorf("unknown tool %q", name)
	}
	if !slices.Contains(spec.Platforms, e.tools.Platform) {
		return &toolError{Code: codeUnsupportedPlatform, Msg: fmt.Sprintf("tool %s is not supported on %s", name, e.tools.Platform)}
	}
	if firstToolExecutable(spec.Detect) != "" {
		return nil
	}
	brew := firstToolExecutable(e.tools.BrewPaths)
	if brew == "" {
		return &toolError{Code: codeHomebrewMissing, Msg: "Homebrew was not found"}
	}
	installCtx, cancel := context.WithTimeout(ctx, e.tools.InstallTimeout)
	defer cancel()
	if err := e.runCommandTail(installCtx, []string{brew, "install", spec.Formula}, 20, 4096); err != nil {
		if errors.Is(installCtx.Err(), context.DeadlineExceeded) {
			return &toolError{Code: codeToolInstallFailed, Msg: fmt.Sprintf("install tool %s timed out", name)}
		}
		return &toolError{Code: codeToolInstallFailed, Msg: fmt.Sprintf("install tool %s: %v", name, err)}
	}
	if firstToolExecutable(spec.Detect) == "" {
		return &toolError{Code: codeToolInstallFailed, Msg: fmt.Sprintf("tool %s was not detected after installation", name)}
	}
	return nil
}

func rpcCodeFor(err error) int {
	var te *toolError
	if errors.As(err, &te) {
		return te.Code
	}
	var re *remoteFrameError
	if errors.As(err, &re) && re.Code != 0 {
		return re.Code
	}
	return -32000
}

func (m *Manager) runInstallTool(ctx context.Context, msg *Message) {
	var p struct {
		Tool string `json:"tool"`
	}
	if !m.parse(msg, &p) {
		return
	}
	if p.Tool == "" {
		m.codec.RespondError(msg.ID, -32602, "tool is required")
		return
	}
	if _, ok := m.exec.tools.Tools[p.Tool]; !ok {
		m.codec.RespondError(msg.ID, -32602, fmt.Sprintf("unknown tool %q", p.Tool))
		return
	}
	if err := m.exec.InstallTool(ctx, p.Tool); err != nil {
		m.codec.RespondError(msg.ID, rpcCodeFor(err), err.Error())
		return
	}
	m.codec.Respond(msg.ID, map[string]any{"tool": p.Tool, "installed": true})
}
