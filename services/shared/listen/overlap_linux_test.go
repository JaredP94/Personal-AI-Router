// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package listen

import (
	"errors"
	"net"
	"strconv"
	"syscall"
	"testing"
)

// Linux must exclude specific/wildcard overlap in both orders: this is why
// TCP can release the overlapping preflights without losing port ownership.
func TestLinuxWildcardExcludesLoopbackInBothOrders(t *testing.T) {
	for _, firstHost := range []string{"", "127.0.0.1"} {
		t.Run(firstHost, func(t *testing.T) {
			first, err := net.Listen("tcp", net.JoinHostPort(firstHost, "0"))
			if err != nil {
				t.Fatal(err)
			}
			defer first.Close()
			secondHost := "127.0.0.1"
			if firstHost != "" {
				secondHost = ""
			}
			second, err := net.Listen("tcp", net.JoinHostPort(secondHost, strconv.Itoa(first.Addr().(*net.TCPAddr).Port)))
			if second != nil {
				second.Close()
			}
			if !errors.Is(err, syscall.EADDRINUSE) {
				t.Fatalf("Linux permitted overlapping listening sockets: %v", err)
			}
		})
	}
}
