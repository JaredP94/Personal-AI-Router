// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// windowsProcessJob gates an engine-manager launcher before it can spawn the
// target. Only the event is inherited; the kill-on-close job and pinned process
// handle remain exclusively in the manager. Both downloads and engines use it.
type windowsProcessJob struct {
	cmd                *exec.Cmd
	job, gate, process windows.Handle
}

func configureProcessJob(cmd *exec.Cmd, breakaway bool) (*processJob, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	native := &windowsProcessJob{cmd: cmd, job: job}
	owner := newProcessJob(native)
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	// Command-mode vendor CLIs can explicitly launch an independent daemon with
	// CREATE_BREAKAWAY_FROM_JOB. Never enable SILENT_BREAKAWAY: ordinary children
	// must remain owned, even when they reparent or detach their console.
	if breakaway {
		limits.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		owner.release()
		return nil, err
	}
	native.gate, err = windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		owner.release()
		return nil, err
	}
	if err := windows.SetHandleInformation(native.gate, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
		owner.release()
		return nil, err
	}
	cmd.Args = append([]string{executable, "--run-download", strconv.FormatUint(uint64(native.gate), 10), cmd.Path}, cmd.Args[1:]...)
	cmd.Path = executable
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NEW_CONSOLE, AdditionalInheritedHandles: []syscall.Handle{syscall.Handle(native.gate)}}
	return owner, nil
}

func (p *windowsProcessJob) assign() error {
	// cmd.Wait cannot run before activation, so os.Process still holds the
	// original process object and its PID cannot have been recycled.
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.cmd.Process.Pid))
	if err != nil {
		return err
	}
	p.process = handle
	if err := windows.AssignProcessToJobObject(p.job, handle); err != nil {
		// The gated launcher has no children yet and is not in our job. Kill only
		// the pinned process object, never a newly looked-up PID.
		_ = windows.TerminateProcess(handle, 1)
		return fmt.Errorf("assign process job: %w", err)
	}
	return nil
}
func (p *windowsProcessJob) resume() error    { return windows.SetEvent(p.gate) }
func (p *windowsProcessJob) terminate() error { return windows.TerminateJobObject(p.job, 1) }
func (p *windowsProcessJob) close() error {
	for _, h := range []windows.Handle{p.gate, p.process} {
		if h != 0 {
			_ = windows.CloseHandle(h)
		}
	}
	return windows.CloseHandle(p.job)
}
