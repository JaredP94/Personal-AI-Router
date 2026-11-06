// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"io"
	"os/exec"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

func startEngineCommand(cmd *exec.Cmd) (*processJob, error) {
	owner, err := configureProcessJob(cmd, false)
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		owner.release()
		return nil, err
	}
	if err := owner.activate(); err != nil {
		_ = cmd.Process.Kill() // The original os.Process handle still proves identity.
		cmd.WaitDelay = time.Second
		_ = cmd.Wait()
		return nil, err
	}
	return owner, nil
}

func waitEngineCommand(mp *managedProc, scans *sync.WaitGroup, stdout, stderr io.Closer) {
	handle, openErr := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(mp.cmd.Process.Pid))
	if openErr == nil {
		defer windows.CloseHandle(handle)
	}
	// Observe exit without releasing cmd's process handle or closing its pipes.
	// Polling waits are bounded; Stop terminates the job to end engine lifetime.
	for openErr == nil {
		status, err := windows.WaitForSingleObject(handle, 1000)
		if err != nil || status != uint32(windows.WAIT_TIMEOUT) {
			break
		}
	}
	mp.owner.release() // Also kill descendants that outlived the launcher.
	drained := make(chan struct{})
	go func() { scans.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(time.Second):
		_ = stdout.Close()
		_ = stderr.Close()
	}
	mp.cmd.WaitDelay = time.Second
	_ = mp.cmd.Wait()
}

func stopEngineCommand(mp *managedProc) {
	_ = mp.owner.stop()
	// Kill-on-close is the final fallback, also on errors from TerminateJobObject.
	mp.owner.release()
	select {
	case <-mp.done:
	case <-time.After(5 * time.Second):
	}
}
