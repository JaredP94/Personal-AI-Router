// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package jsonrpc

import (
	"golang.org/x/sys/unix"
	"os"
)

func interruptibleStdioFile(f *os.File) (*os.File, error) {
	fd, err := unix.FcntlInt(f.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	// NewFile recognizes O_NONBLOCK and registers pipes with the runtime poller.
	replacement := os.NewFile(uintptr(fd), f.Name())
	_ = f.Close() // no original descriptor may keep the dead stream alive
	return replacement, nil
}
