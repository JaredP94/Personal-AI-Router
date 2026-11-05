// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type downloadProcess struct {
	cmd       *exec.Cmd
	job, gate windows.Handle
}

func (p *downloadProcess) close() {
	_ = windows.CloseHandle(p.gate)
	_ = windows.CloseHandle(p.job) // KILL_ON_JOB_CLOSE also covers worker crashes.
}

func configureDownloadProcess(cmd *exec.Cmd) (*downloadProcess, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	p := &downloadProcess{cmd: cmd, job: job}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		p.close()
		return nil, err
	}
	p.gate, err = windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		p.close()
		return nil, err
	}
	handle := p.gate
	if err := windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
		p.close()
		return nil, err
	}
	// Keep the private-console launcher, but gate CLI creation until the
	// launcher belongs to its job: no child can escape an assignment race.
	cmd.Args = append([]string{executable, "--run-download", strconv.FormatUint(uint64(handle), 10), cmd.Path}, cmd.Args[1:]...)
	cmd.Path = executable
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow: true, CreationFlags: windows.CREATE_NEW_CONSOLE,
		AdditionalInheritedHandles: []syscall.Handle{syscall.Handle(handle)},
	}
	return p, nil
}

func killDownload(_ context.Context, process *downloadProcess) error {
	return windows.TerminateJobObject(process.job, 1)
}
func terminateDownload(ctx context.Context, process *downloadProcess) error {
	return interruptDownload(ctx, process)
}

// JOBOBJECT_BASIC_ACCOUNTING_INFORMATION, including the entire descendant tree.
// x/sys exposes its information class but not this structure.
type downloadJobAccounting struct {
	totalUserTime, totalKernelTime, periodUserTime, periodKernelTime      int64
	totalPageFaults, totalProcesses, activeProcesses, terminatedProcesses uint32
}

func waitDownloadGroup(ctx context.Context, process *downloadProcess) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var accounting downloadJobAccounting
		if err := windows.QueryInformationJobObject(process.job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&accounting)), uint32(unsafe.Sizeof(accounting)), nil); err != nil {
			return fmt.Errorf("query download job: %w", err)
		}
		if accounting.activeProcesses == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("download job still has %d active processes: %w", accounting.activeProcesses, ctx.Err())
		case <-ticker.C:
		}
	}
}

// pinDownloadProcess holds a handle to the launcher so Windows cannot reissue
// its PID while this worker may still signal it. The handle os.Process owns is
// released once the launcher is reaped, but cmd.Wait then blocks for up to
// downloadWaitDelay on pipes a grandchild still holds. The interrupt helper
// addresses that PID, whereas forced cleanup addresses the job. Assign the
// job before opening the start gate and before cmd.Wait can reap the launcher.
func pinDownloadProcess(process *downloadProcess) (func(), error) {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(process.cmd.Process.Pid))
	if err != nil {
		return nil, err
	}
	if err := windows.AssignProcessToJobObject(process.job, handle); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	if err := windows.SetEvent(process.gate); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	return func() { _ = windows.CloseHandle(handle) }, nil
}

// runDownloadProcess owns the CLI in a private hidden console. Reset only this
// launcher's inherited Ctrl+C setting, leaving the broker and worker untouched.
func runDownloadProcess(gate windows.Handle, argv []string) int {
	status, err := windows.WaitForSingleObject(gate, 2000)
	_ = windows.CloseHandle(gate) // No gate or job handle reaches CLI children.
	if err != nil || status != windows.WAIT_OBJECT_0 {
		return 1
	}
	ignore := windows.NewLazySystemDLL("kernel32.dll").NewProc("SetConsoleCtrlHandler")
	if ok, _, err := ignore.Call(0, 0); ok == 0 {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// The child has inherited enabled Ctrl+C. Ignore it in the launcher only,
	// so the launcher stays alive to relay the CLI's exit and close its pipes.
	if ok, _, err := ignore.Call(0, 1); ok == 0 {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// openDownloadLauncher returns an open handle to pid when it still names a
// launcher this worker started, established by its image being this same
// executable. A cancel racing the CLI's exit could otherwise attach to a
// stranger's console — and GenerateConsoleCtrlEvent(CTRL_C_EVENT, 0) reaches
// everything sharing it.
//
// The handle is the guard, not the image check. Windows reissues a PID as soon
// as the last handle to the exited process closes, so verifying the image and
// then closing the handle proves only what was true a moment ago: the PID can
// be recycled before the caller attaches. Holding this handle keeps the
// process object, and therefore the PID, from being reused — so the caller
// must not close it until it has attached to the console.
func openDownloadLauncher(pid uint32) (windows.Handle, bool) {
	self, err := os.Executable()
	if err != nil {
		return 0, false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0, false
	}
	image := make([]uint16, 32768)
	size := uint32(len(image))
	if err := windows.QueryFullProcessImageName(handle, 0, &image[0], &size); err != nil {
		_ = windows.CloseHandle(handle)
		return 0, false
	}
	if !strings.EqualFold(windows.UTF16ToString(image[:size]), self) {
		_ = windows.CloseHandle(handle)
		return 0, false
	}
	return handle, true
}

func interruptDownload(parent context.Context, process *downloadProcess) error {
	cmd := process.cmd
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	helper := exec.CommandContext(ctx, executable, "--interrupt-download", strconv.Itoa(cmd.Process.Pid))
	configureSysProcAttr(helper)
	return helper.Run()
}

// Download launch and console attachment run in helpers so engine-manager's
// stdio and signal handlers are not affected.
func handleDownloadProcess() bool {
	if len(os.Args) >= 4 && os.Args[1] == "--run-download" {
		gate, err := strconv.ParseUint(os.Args[2], 10, 64)
		if err != nil || gate == 0 {
			os.Exit(1)
		}
		os.Exit(runDownloadProcess(windows.Handle(gate), os.Args[3:]))
	}
	if len(os.Args) != 3 || os.Args[1] != "--interrupt-download" {
		return false
	}
	pid, err := strconv.ParseUint(os.Args[2], 10, 32)
	if err != nil || pid == 0 {
		os.Exit(1)
	}
	launcher, ok := openDownloadLauncher(uint32(pid))
	if !ok {
		fmt.Fprintln(os.Stderr, "download launcher is gone")
		os.Exit(1)
	}
	kernel := windows.NewLazySystemDLL("kernel32.dll")
	attach := kernel.NewProc("AttachConsole")
	free := kernel.NewProc("FreeConsole")
	ignore := kernel.NewProc("SetConsoleCtrlHandler")
	generate := kernel.NewProc("GenerateConsoleCtrlEvent")
	free.Call()
	attached, _, attachErr := attach.Call(uintptr(pid))
	// The handle has held the PID against reuse up to here, which is the point
	// the console stops being addressed by number, so it has done its job.
	// os.Exit below would skip a defer, so close it explicitly.
	_ = windows.CloseHandle(launcher)
	if attached == 0 {
		fmt.Fprintln(os.Stderr, attachErr)
		os.Exit(1)
	}
	if ok, _, err := ignore.Call(0, 1); ok == 0 {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if ok, _, err := generate.Call(0, 0); ok == 0 {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Control-event delivery is asynchronous.
	time.Sleep(100 * time.Millisecond)
	free.Call()
	return true
}
