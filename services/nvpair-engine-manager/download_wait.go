// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os/exec"
	"sync"
)

// downloadWait separates launcher exit notification from reaping and stdio drain.
type downloadWait struct {
	cmd              *exec.Cmd
	exited, finished chan struct{}
	once             sync.Once
	err              error
	exitErr          error // Published by closing exited.
}

func newDownloadWait(cmd *exec.Cmd) *downloadWait {
	return &downloadWait{cmd: cmd, exited: make(chan struct{}), finished: make(chan struct{})}
}

func (w *downloadWait) reap() {
	w.once.Do(func() {
		go func() { w.err = w.cmd.Wait(); close(w.finished) }()
	})
}
