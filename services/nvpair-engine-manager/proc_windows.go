// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// configureSysProcAttr hides the child's console window
// (HideWindow + CREATE_NO_WINDOW), matching every other NVPAIR subprocess.
func configureSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}

var (
	iphlpapi                = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTcpTable = iphlpapi.NewProc("GetExtendedTcpTable")
)

const (
	// TCP_TABLE_OWNER_PID_LISTENER — listening sockets with their owning PID.
	tcpTableOwnerPIDListener = 3
	errInsufficientBuffer    = 122 // ERROR_INSUFFICIENT_BUFFER
)

// mibTCPRowOwnerPID mirrors MIB_TCPROW_OWNER_PID (IPv4). Every field is a
// DWORD, so the C layout is a run of uint32s with no padding.
type mibTCPRowOwnerPID struct {
	state      uint32
	localAddr  uint32
	localPort  uint32
	remoteAddr uint32
	remotePort uint32
	owningPID  uint32
}

// mibTCP6RowOwnerPID mirrors MIB_TCP6ROW_OWNER_PID (IPv6).
type mibTCP6RowOwnerPID struct {
	localAddr     [16]byte
	localScopeID  uint32
	localPort     uint32
	remoteAddr    [16]byte
	remoteScopeID uint32
	remotePort    uint32
	state         uint32
	owningPID     uint32
}

// pidOnPort returns the PID of the process listening on the given TCP port
// (IPv4 first, then IPv6) plus that process's full image path. ok is false
// when nothing is listening or the owner can't be resolved, so a caller can
// fail closed (decline the stop). It is precise to a single port: a genuine
// desktop app on a different port is never surfaced.
func pidOnPort(port int) (pid int, image string, ok bool) {
	if p, found := listenerPID(windows.AF_INET, port); found {
		return p, imagePathForPID(p), true
	}
	if p, found := listenerPID(windows.AF_INET6, port); found {
		return p, imagePathForPID(p), true
	}
	return 0, "", false
}

// listenerPID walks the owner-PID TCP table for one address family and
// returns the PID whose local port matches. The table is fetched with the
// standard two-call sizing dance (NULL buffer to learn the size, then a
// real buffer).
func listenerPID(family uint32, port int) (int, bool) {
	var size uint32
	r, _, _ := procGetExtendedTcpTable.Call(0, uintptr(unsafe.Pointer(&size)), 0, uintptr(family), tcpTableOwnerPIDListener, 0)
	if r != errInsufficientBuffer && r != 0 {
		return 0, false
	}
	if size == 0 {
		return 0, false
	}
	buf := make([]byte, size)
	r, _, _ = procGetExtendedTcpTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, uintptr(family), tcpTableOwnerPIDListener, 0)
	if r != 0 {
		return 0, false
	}
	// Both MIB_*TABLE_OWNER_PID structs are { DWORD dwNumEntries; row[] }, so
	// the rows begin at offset 4 (the row structs are 4-byte aligned).
	n := *(*uint32)(unsafe.Pointer(&buf[0]))
	if n == 0 {
		return 0, false
	}
	if family == windows.AF_INET {
		rows := unsafe.Slice((*mibTCPRowOwnerPID)(unsafe.Pointer(&buf[4])), int(n))
		for i := range rows {
			if ntohsPort(rows[i].localPort) == port {
				return int(rows[i].owningPID), true
			}
		}
		return 0, false
	}
	rows := unsafe.Slice((*mibTCP6RowOwnerPID)(unsafe.Pointer(&buf[4])), int(n))
	for i := range rows {
		if ntohsPort(rows[i].localPort) == port {
			return int(rows[i].owningPID), true
		}
	}
	return 0, false
}

// ntohsPort converts the local-port DWORD (the port number in network byte
// order, held in the low 16 bits) to a host-order int.
func ntohsPort(dw uint32) int {
	return int(((dw & 0xFF) << 8) | ((dw >> 8) & 0xFF))
}

// imagePathForPID resolves a PID's full executable path. Empty on any error
// (access denied, exited) so the image check fails closed.
func imagePathForPID(pid int) string {
	if pid <= 0 {
		return ""
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, 1024)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:size])
}

// A legacy orphan has no job. Revalidate the expected PAIR-installed image
// through a pinned handle and terminate only that process object. Enumerating
// and killing a PID tree cannot establish ownership of its current children.
// The caller has already checked that expectedImage is in an owned install.
func terminateOwnedPID(pid int, grace time.Duration, expectedImage string) {
	if pid <= 0 || expectedImage == "" {
		return
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, 32768)
	size := uint32(len(buf))
	if windows.QueryFullProcessImageName(h, 0, &buf[0], &size) != nil || !isOurEngineImage(windows.UTF16ToString(buf[:size]), expectedImage) {
		return
	}
	// Windowless legacy engines cannot receive WM_CLOSE. Unlike owned jobs,
	// there is no safe way to reclaim descendants without their own proof.
	if windows.TerminateProcess(h, 1) != nil {
		return
	}
	if grace <= 0 {
		grace = 5 * time.Second
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		status, err := windows.WaitForSingleObject(h, 100)
		if err != nil || status != uint32(windows.WAIT_TIMEOUT) {
			return
		}
	}
}

// pidAlive reports whether the PID is still running (WaitForSingleObject with
// a zero timeout: WAIT_TIMEOUT means it hasn't signaled exit). A handle we
// can't open is treated as gone.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	s, err := windows.WaitForSingleObject(h, 0)
	if err != nil {
		return true
	}
	return s == uint32(windows.WAIT_TIMEOUT)
}
