// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package listen

import (
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestTCPRejectsForeignLoopback(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1"} {
		t.Run(host, func(t *testing.T) {
			foreign, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
			if err != nil {
				if host == "::1" && ipv6Unavailable(err) {
					t.Skipf("IPv6 loopback unavailable: %v", err)
				}
				t.Fatal(err)
			}
			defer foreign.Close()
			port := foreign.Addr().(*net.TCPAddr).Port
			ln, err := TCP(":" + strconv.Itoa(port))
			if ln != nil {
				ln.Close()
			}
			if !errors.Is(err, syscall.EADDRINUSE) || !strings.Contains(err.Error(), foreign.Addr().String()) {
				t.Fatalf("TCP with foreign %s: got %v, want EADDRINUSE naming owner", foreign.Addr(), err)
			}
			// Failure must not leave any of the earlier reservation sockets behind.
			foreign.Close()
			retry, err := TCP(":" + strconv.Itoa(port))
			if err != nil {
				t.Fatalf("partial bind leaked: %v", err)
			}
			retry.Close()
		})
	}
}

func TestTCPPortZeroAcceptsAllAddresses(t *testing.T) {
	for _, wildcard := range []string{":0", "0.0.0.0:0"} {
		t.Run(wildcard, func(t *testing.T) {
			ln, err := TCP(wildcard)
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			addr := ln.Addr().(*net.TCPAddr)
			if addr.Port == 0 || !addr.IP.IsUnspecified() {
				t.Fatalf("not a resolved wildcard address: %v", addr)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					c.SetDeadline(time.Now().Add(3 * time.Second))
					c.Write([]byte("PAIR"))
					c.Close()
				}
			}()
			hosts := []string{"127.0.0.1"}
			probe, err := net.Listen("tcp6", "[::1]:0")
			if err == nil {
				probe.Close()
				hosts = append(hosts, "::1")
			} else if !ipv6Unavailable(err) {
				t.Fatal(err)
			}
			interfaces, err := net.InterfaceAddrs()
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, a := range interfaces {
				ip, _, err := net.ParseCIDR(a.String())
				if err == nil && ip.To4() != nil && !ip.IsLoopback() {
					hosts = append(hosts, ip.String())
					found = true
					break
				}
			}
			if !found {
				t.Fatal("no non-loopback IPv4 address for wildcard reachability test")
			}
			for _, host := range hosts {
				c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(addr.Port)), 3*time.Second)
				if err != nil {
					t.Fatalf("dial %s: %v", host, err)
				}
				c.SetDeadline(time.Now().Add(3 * time.Second))
				b := make([]byte, 4)
				_, err = io.ReadFull(c, b)
				c.Close()
				if err != nil || string(b) != "PAIR" {
					t.Fatalf("%s reached wrong listener: %q, %v", host, b, err)
				}
			}
			ln.Close()
			<-done
			for _, host := range hosts {
				rebound, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(addr.Port)))
				if err != nil {
					t.Fatalf("Close leaked %s: %v", host, err)
				}
				rebound.Close()
			}
			rebound, err := TCP(net.JoinHostPort("", strconv.Itoa(addr.Port)))
			if err != nil {
				t.Fatalf("Close leaked wildcard: %v", err)
			}
			rebound.Close()
		})
	}
}

func TestTCPClosePreservesAcceptedConnection(t *testing.T) {
	ln, err := TCP(":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	server.SetDeadline(time.Now().Add(time.Second))
	client.SetDeadline(time.Now().Add(time.Second))
	if _, err := server.Write([]byte("drain")); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 5)
	if _, err := io.ReadFull(client, data); err != nil || string(data) != "drain" {
		t.Fatalf("Close interrupted handed-off connection: %q %v", data, err)
	}
}

func TestTCPCloseUnblocksAccept(t *testing.T) {
	ln, err := TCP(":0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := ln.Accept(); done <- err }()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := ln.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept after Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Accept stuck after Close")
	}
}
