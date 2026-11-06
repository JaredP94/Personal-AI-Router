// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// managedProc is a spawned engine process with its stdout/stderr
// captured line-by-line and a `done` channel that closes when it
// exits. The OS-specific primitives it relies on (console hiding,
// graceful signal, force kill) live in proc_windows.go / proc_unix.go
// so the same logic here runs on every platform.
type managedProc struct {
	cmd   *exec.Cmd
	done  chan struct{}
	owner *processJob // Windows only; Unix retains its existing process groups.
}

// startManagedProc launches bin with args and the extra env (merged
// over the current environment), hides any console window, and streams
// stdout/stderr lines to onLine. The returned proc's done channel
// closes once the process exits.
func startManagedProc(bin string, args []string, env map[string]string, onLine func(stream, line string)) (*managedProc, error) {
	cmd := exec.Command(bin, args...)
	if len(env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	configureSysProcAttr(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	owner, err := startEngineCommand(cmd)
	if err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, err
	}

	mp := &managedProc{cmd: cmd, done: make(chan struct{}), owner: owner}
	var scanWg sync.WaitGroup
	scanWg.Add(2)
	go func() { defer scanWg.Done(); scanLines(stdout, "stdout", onLine) }()
	go func() { defer scanWg.Done(); scanLines(stderr, "stderr", onLine) }()
	go func() {
		// Drain stdout/stderr fully before Wait: Wait closes the pipes on
		// exit, which would otherwise truncate the final captured lines.
		waitEngineCommand(mp, &scanWg, stdout, stderr)
		close(mp.done)
	}()
	return mp, nil
}

func scanLines(r io.Reader, stream string, onLine func(stream, line string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 256*1024)
	for sc.Scan() {
		if onLine != nil {
			onLine(stream, sc.Text())
		}
	}
}

// stop stops the process using the platform's ownership-aware stop path.
//
// It sends one platform-appropriate stop signal (see gracefulSignal) and then
// blocks until the process is gone:
//   - Unix: SIGTERM to the process group — a graceful ask, with no escalation
//     to SIGKILL. A well-behaved engine (Ollama, and the test fake) exits on it.
//   - Windows: terminate the owned job; no PID lookup or tree enumeration.
//     Command-mode engines use their bounded official stop CLI separately.
//
// Unix deliberately retains its existing unbounded SIGTERM wait. Windows
// bounds its final exit/drain wait after terminating and closing the owned job.
func (mp *managedProc) stop() {
	if mp == nil || mp.cmd == nil || mp.cmd.Process == nil {
		return
	}
	select {
	case <-mp.done:
		return // already exited
	default:
	}
	stopEngineCommand(mp)
}

// normalizeEngineImage cleans an executable path for comparison. Linux
// /proc/<pid>/exe can suffix " (deleted)" when the file was replaced while
// the process still runs.
func normalizeEngineImage(path string) string {
	return filepath.Clean(strings.TrimSuffix(path, " (deleted)"))
}

// canonicalEnginePath is normalizeEngineImage with symlinks resolved. macOS
// reports a running image by its resolved path (/private/var/...) while the
// configured path may go through a link (/var/...). Use it only to compare,
// never on its own to decide ownership: resolving an install directory that is
// itself a link would make whatever it points at look managed.
func canonicalEnginePath(path string) string {
	path = normalizeEngineImage(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// isOurEngineImage reports whether the listener on our managed port is
// running the same binary this service manages for the engine (st.binPath).
// The compare is path-cleaned and case-insensitive so a PAIR-owned orphan on
// our managed port is reclaimed by Stop, while an unrelated process that
// merely grabbed the port is left for the caller to decline. An empty image
// (e.g. a PID we can't introspect) never matches, so Stop fails closed.
func isOurEngineImage(image, binPath string) bool {
	if image == "" || binPath == "" {
		return false
	}
	return strings.EqualFold(canonicalEnginePath(image), canonicalEnginePath(binPath))
}

// isManagedInstallPath reports whether binPath is inside this engine's
// NVPAIR-owned install directory. A matching executable name/path is not, by
// itself, ownership: Detect intentionally recognizes vendor/user installs on
// some platforms. Stop/uninstall must never reclaim those external files.
func isManagedInstallPath(binPath, installDir string) bool {
	if binPath == "" || installDir == "" {
		return false
	}
	binAbs, err := filepath.Abs(normalizeEngineImage(binPath))
	if err != nil {
		return false
	}
	dirAbs, err := filepath.Abs(installDir)
	if err != nil {
		return false
	}
	return pathUnder(dirAbs, binAbs)
}

// isOwnedInstallPath is the ownership test for stopping or removing an engine:
// binPath must be inside the install directory both as written and with every
// link resolved. As written alone, a link inside the install directory could
// lead to an external binary; resolved alone, an install directory that is
// itself a link would claim whatever it points at.
func isOwnedInstallPath(binPath, installDir string) bool {
	if !isManagedInstallPath(binPath, installDir) {
		return false
	}
	// PAIR creates the install directory itself. One that is a link leads
	// somewhere PAIR did not install into, so nothing reached through it is
	// PAIR's, however the path is spelled. Links above it (/var on macOS)
	// are the system's and are fine.
	if info, err := os.Lstat(filepath.Clean(installDir)); err != nil || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	binAbs, err := filepath.Abs(canonicalEnginePath(binPath))
	if err != nil {
		return false
	}
	dirAbs, err := filepath.Abs(canonicalEnginePath(installDir))
	if err != nil {
		return false
	}
	return pathUnder(dirAbs, binAbs)
}

// pathUnder reports whether path is dir or inside it, by whole components.
func pathUnder(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}
