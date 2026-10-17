// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package netmon

// macOS backend: open a PF_SYSTEM kernel-event socket filtered to Apple
// network events (KEV_VENDOR_APPLE / KEV_NETWORK_CLASS), which the kernel
// emits for IPv4/IPv6 address additions/changes/deletions and data-link
// state changes without delivering high-frequency routing-table cache-miss
// (RTM_MISS) traffic. If the kernel event socket cannot be opened or filtered,
// fall back to a PF_ROUTE socket that filters for RTM_NEWADDR / RTM_DELADDR /
// RTM_IFINFO / RTM_IFINFO2 before signaling the Monitor.

import (
	"context"
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	sysprotoEvent    = 1          // SYSPROTO_EVENT (<sys/sys_domain.h>)
	siocskevfilt     = 0x800c6502 // _IOW('e', 2, struct kev_request) (<sys/kern_event.h>)
	kevVendorApple   = 1          // KEV_VENDOR_APPLE
	kevNetworkClass  = 1          // KEV_NETWORK_CLASS
	kevAnySubclass   = 0          // KEV_ANY_SUBCLASS
	kevInetSubclass  = 1          // KEV_INET_SUBCLASS (<net/net_kev.h>)
	kevDLSubclass    = 2          // KEV_DL_SUBCLASS (<net/net_kev.h>)
	kevInet6Subclass = 6          // KEV_INET6_SUBCLASS (<net/net_kev.h>)
)

type kevRequest struct {
	vendorCode  uint32
	kevClass    uint32
	kevSubclass uint32
}

func watchChanges(ctx context.Context, onEvent func()) {
	if watchKernEvents(ctx, onEvent) {
		return
	}
	watchRouteSocket(ctx, onEvent)
}

func watchKernEvents(ctx context.Context, onEvent func()) bool {
	fd, err := unix.Socket(unix.AF_SYSTEM, unix.SOCK_RAW, sysprotoEvent)
	if err != nil {
		return false
	}
	req := kevRequest{
		vendorCode:  kevVendorApple,
		kevClass:    kevNetworkClass,
		kevSubclass: kevAnySubclass,
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(siocskevfilt), uintptr(unsafe.Pointer(&req)))
	if errno != 0 {
		_ = unix.Close(fd)
		return false
	}
	_ = unix.SetNonblock(fd, true)
	f := os.NewFile(uintptr(fd), "kev")
	if f == nil {
		_ = unix.Close(fd)
		return false
	}
	stop := context.AfterFunc(ctx, func() {
		_ = f.Close()
	})
	defer func() {
		stop()
		_ = f.Close()
	}()

	buf := make([]byte, 4096)
	for {
		if ctx.Err() != nil {
			return true
		}
		n, err := f.Read(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, os.ErrClosed) {
				return true
			}
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return false
		}
		if n < 24 {
			continue
		}
		// struct kern_event_msg layout (<sys/kern_event.h>):
		// [0:4] total_size, [4:8] vendor_code, [8:12] kev_class,
		// [12:16] kev_subclass, [16:20] id, [20:24] event_code.
		subclass := *(*uint32)(unsafe.Pointer(&buf[12]))
		eventCode := *(*uint32)(unsafe.Pointer(&buf[20]))
		switch subclass {
		case kevInetSubclass:
			// KEV_INET_NEW_ADDR (1), KEV_INET_CHANGED_ADDR (2), KEV_INET_ADDR_DELETED (3)
			if eventCode >= 1 && eventCode <= 6 {
				onEvent()
			}
		case kevInet6Subclass:
			// KEV_INET6_NEW_USER_ADDR (1) .. KEV_INET6_DEFROUTER (6)
			if eventCode >= 1 && eventCode <= 6 {
				onEvent()
			}
		case kevDLSubclass:
			// KEV_DL_SIFFLAGS (1), KEV_DL_IF_ATTACHED (9)..KEV_DL_LINK_ADDRESS_CHANGED (16), KEV_DL_PRIMARY_ELECTED (23)
			if eventCode == 1 || (eventCode >= 9 && eventCode <= 16) || eventCode == 23 {
				onEvent()
			}
		}
	}
}

func watchRouteSocket(ctx context.Context, onEvent func()) {
	fd, err := unix.Socket(unix.AF_ROUTE, unix.SOCK_RAW, unix.AF_UNSPEC)
	if err != nil {
		poll(ctx, onEvent)
		return
	}
	_ = unix.SetNonblock(fd, true)
	f := os.NewFile(uintptr(fd), "route")
	if f == nil {
		_ = unix.Close(fd)
		poll(ctx, onEvent)
		return
	}
	stop := context.AfterFunc(ctx, func() {
		_ = f.Close()
	})
	defer func() {
		stop()
		_ = f.Close()
	}()

	buf := make([]byte, 4096)
	for {
		if ctx.Err() != nil {
			return
		}
		n, err := f.Read(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, os.ErrClosed) {
				return
			}
			if errors.Is(err, unix.EINTR) {
				continue
			}
			poll(ctx, onEvent)
			return
		}
		if n < 4 {
			continue
		}
		// struct rt_msghdr: [0:2] rtm_msglen, [2] rtm_version, [3] rtm_type.
		switch buf[3] {
		case unix.RTM_NEWADDR, unix.RTM_DELADDR, unix.RTM_IFINFO, unix.RTM_IFINFO2:
			onEvent()
		}
	}
}
