// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
)

// procImage resolves a PID's executable path with lsof.
//
// macOS has no /proc, and this decides whether a listener on PAIR's managed
// port is PAIR's own engine binary. Without it the check failed closed every
// time, so a stop or uninstall of an engine PAIR started but lost the handle to
// (after a crash, a force quit or a worker restart) was always refused.
//
// The `txt` descriptor is the running image as the kernel records it, not argv,
// so it is right for a process launched through a PATH lookup and cannot be
// spoofed by the process renaming itself. `ps -o comm=` is not a substitute: it
// reports a bare name in exactly that case.
//
// Empty on any error, so the caller's image check fails closed.
func procImage(pid int) string {
	if pid <= 0 {
		return ""
	}
	lsof := systemTool("lsof", lsofLocations)
	if lsof == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), portLookupTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, lsof, "-w", "-p", strconv.Itoa(pid), "-Fn", "-a", "-d", "txt")
	// lsof escapes non-ASCII bytes in names unless the locale says UTF-8.
	cmd.Env = []string{"LC_ALL=en_US.UTF-8"}
	out, _ := cmd.Output()
	return parseLsofImage(string(out))
}

// parseLsofImage extracts the executable from lsof's -F field output for txt
// descriptors: an `f` line names a descriptor and the `n` line after it is its
// path. The kernel lists the executable before the libraries it loaded, so the
// first txt path is the image.
func parseLsofImage(out string) string {
	inText := false
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "f"):
			inText = line == "ftxt"
		case inText && strings.HasPrefix(line, "n"):
			// Only a stray CR is stripped: a path really ending in a space is
			// reported verbatim, and trimming it would let "/dir/engine "
			// match the managed "/dir/engine" in a check that authorizes a kill.
			return strings.TrimSuffix(line[1:], "\r")
		}
	}
	return ""
}
