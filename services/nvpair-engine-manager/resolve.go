// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Resolution is the one-shot executable contract, not a lifecycle operation.
// Env contains only the manifest's runtime additions to the caller's environment.
type Resolution struct {
	Engine string            `json:"engine"`
	Path   string            `json:"path"`
	Env    map[string]string `json:"env"`
}

type listedEngine struct {
	Engine      string `json:"engine"`
	DisplayName string `json:"display_name"`
	Installed   bool   `json:"installed"`
	Supported   bool   `json:"supported"`
}

// detectedExecutable is shared by detection and command-mode CLI execution.
// An explicit runtime.cli is authoritative for command mode; otherwise detect
// declares the ordered CLI candidates. No PATH lookup or undeclared locations.
func detectedExecutable(plat *Platform, installDir, goos string) string {
	paths := plat.Detect
	if plat.Runtime.modeOrDefault() == "command" && plat.Runtime.CLI != "" {
		paths = []string{plat.Runtime.CLI}
	}
	for _, path := range paths {
		resolved, err := resolvePlaceholders(path, map[string]string{"install_dir": installDir})
		if err != nil {
			continue
		}
		expanded := expandPathForOS(resolved, goos)
		if info, err := os.Stat(expanded); err == nil && !info.IsDir() {
			return expanded
		}
	}
	return ""
}

// controlCLIPath resolves {cli} for lifecycle and actions using the same
// candidates as installed detection and one-shot resolution. Process-mode
// manifests retain their independent optional control CLI.
func controlCLIPath(plat *Platform, installDir, goos string) string {
	if plat.Runtime.modeOrDefault() == "command" {
		return detectedExecutable(plat, installDir, goos)
	}
	return expandPathForOS(plat.Runtime.CLI, goos)
}

func resolveEngine(reg *Registry, engine, goos, goarch string) (Resolution, error) {
	m, ok := reg.Get(engine)
	if !ok {
		return Resolution{}, fmt.Errorf("unknown engine %q", engine)
	}
	plat, ok := m.PlatformFor(goos, goarch)
	if !ok {
		return Resolution{}, fmt.Errorf("%s is not supported on %s/%s", engine, goos, goarch)
	}
	_, base := userPaths()
	if base == "" {
		return Resolution{}, fmt.Errorf("cannot determine engine installation directory")
	}
	installDir := filepath.Join(base, engine)
	path := detectedExecutable(plat, installDir, goos)
	if path == "" {
		if externallyAvailable(plat) {
			return Resolution{}, fmt.Errorf("%s is running as an external service; PAIR has no executable for it", engine)
		}
		return Resolution{}, fmt.Errorf("%s is not installed", engine)
	}
	vars := map[string]string{
		"host": "127.0.0.1", "port": strconv.Itoa(plat.Runtime.Port), "install_dir": installDir,
		"bin": path,
	}
	if plat.Runtime.modeOrDefault() == "command" {
		vars["cli"] = path
	} else if plat.Runtime.CLI != "" {
		cli, err := resolvePlaceholders(plat.Runtime.CLI, vars)
		if err != nil {
			return Resolution{}, err
		}
		vars["cli"] = expandPathForOS(cli, goos)
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return Resolution{}, fmt.Errorf("%s is not installed (executable is missing: %s)", engine, path)
	}
	env := make(map[string]string, len(plat.Runtime.Env))
	for key, value := range plat.Runtime.Env {
		resolved, err := resolvePlaceholders(value, vars)
		if err != nil {
			return Resolution{}, err
		}
		env[key] = resolved
	}
	return Resolution{Engine: engine, Path: path, Env: env}, nil
}

func listEngines(reg *Registry, goos, goarch string) []listedEngine {
	_, base := userPaths()
	out := make([]listedEngine, 0, len(reg.Names()))
	for _, name := range reg.Names() {
		m, _ := reg.Get(name)
		entry := listedEngine{Engine: name, DisplayName: m.DisplayName}
		if plat, ok := m.PlatformFor(goos, goarch); ok {
			entry.Supported = true
			entry.Installed = base != "" && detectedExecutable(plat, filepath.Join(base, name), goos) != ""
			if !entry.Installed {
				entry.Installed = externallyAvailable(plat)
			}
		}
		out = append(out, entry)
	}
	return out
}

// externallyAvailable applies the service-only adoption criterion without
// creating live service state. Only process-mode engines with a fixed port and
// a successful readiness probe qualify. One attempt is bounded to 500 ms,
// including response headers; manifest startup timeouts are intentionally ignored.
func externallyAvailable(plat *Platform) bool {
	rt := plat.Runtime
	if rt.modeOrDefault() != "process" || rt.Port <= 0 || rt.Ready == nil {
		return false
	}
	addr, err := probeListenerAddress(rt.Ready, rt.Port)
	if err != nil {
		return false
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return false
	}
	addr = net.JoinHostPort(host, port)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	// Pin the actual dial target, not just the URL's hostname: no DNS, proxy,
	// or redirect may turn a read-only identity check into a LAN request.
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	ready := *rt.Ready
	if ready.HTTP == "" {
		ready.TCP = addr
	}
	// Reuse the service's readiness/status check (including its identity-probe
	// header), but not reconciliation, which mutates runtime state.
	return (&Executor{client: client}).probe(ctx, &ready, rt.Port)
}
