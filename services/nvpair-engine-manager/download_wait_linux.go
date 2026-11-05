// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"errors"

	"golang.org/x/sys/unix"
)

func watchDownloadExit(w *downloadWait) error {
	go func() {
		var info unix.Siginfo
		for {
			// WNOWAIT leaves the exited launcher waitable: its zombie pins the
			// PID/pgid until the final group signal, even with no children.
			err := unix.Waitid(unix.P_PID, w.cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			w.exitErr = err
			close(w.exited)
			return
		}
	}()
	return nil
}
