// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestForeignLoopbackRejectsStartupAndSetPort(t *testing.T) {
	redirectConfigDir(t)
	foreign, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	occupied := foreign.Addr().(*net.TCPAddr).Port
	t.Run("startup", func(t *testing.T) {
		p := NewProxy(NewCodec(&bytes.Buffer{}), NewDiscovery(), occupied)
		ln, err := p.listen()
		if ln != nil {
			ln.Close()
		}
		if !errors.Is(err, syscall.EADDRINUSE) || !strings.Contains(err.Error(), foreign.Addr().String()) {
			t.Fatalf("startup accepted foreign listener or lost conflict address: %v", err)
		}
	})
	t.Run("set-port", func(t *testing.T) {
		notifications := &bytes.Buffer{}
		p := NewProxy(NewCodec(notifications), NewDiscovery(), 0)
		ln, err := p.listen()
		if err != nil {
			t.Fatal(err)
		}
		previous := ln.Addr().(*net.TCPAddr).Port
		p.port = previous
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		p.serveHTTP(ctx, ln)
		defer p.shutdown(context.Background())
		if err := savePersistedPort(previous); err != nil {
			t.Fatal(err)
		}
		err = p.setPort(occupied)
		if !errors.Is(err, syscall.EADDRINUSE) || !strings.Contains(err.Error(), foreign.Addr().String()) {
			t.Fatalf("set-port accepted foreign listener or lost conflict address: %v", err)
		}
		if p.port != previous {
			t.Fatalf("failed rebind changed port to %d", p.port)
		}
		if saved, ok := loadPersistedPort(); !ok || saved != previous {
			t.Fatalf("failed rebind persisted %d", saved)
		}
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(previous)), time.Second)
		if err != nil {
			t.Fatalf("old listener lost: %v", err)
		}
		c.Close()
		if strings.Contains(notifications.String(), "\"port\":"+strconv.Itoa(occupied)) {
			t.Fatal("announced occupied port")
		}
	})
}
