// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package listen reserves wildcard TCP ports without allowing an existing
// loopback listener to silently receive this service's local traffic.
//
// Bind order is IPv4 loopback, IPv6 loopback (when available), then the original
// wildcard address. On Darwin, Go's SO_REUSEADDR permits wildcard and specific
// listeners to coexist, in either order; keeping the specific sockets both
// detects existing owners and routes loopback connections into the same server.
// Windows permits this overlap by default; Go deliberately does not enable
// SO_REUSEADDR there because it could steal another listener's address. We use
// its normal options and the same specific-first order, never SO_REUSEPORT.
//
// Linux does not permit overlapping listening sockets even with SO_REUSEADDR.
// There the loopback reservations are preflights: release the overlapping ones
// immediately before the wildcard bind, which atomically excludes any competing
// listener (including one that wins the preflight race). Go may choose a
// dual-stack socket even for "0.0.0.0"; inspect the actual wildcard Addr, not the
// input spelling. If it is IPv4-only, reacquire the non-overlapping IPv6 loopback
// socket after the wildcard. Thus Linux's wildcard itself owns covered loopbacks;
// Darwin and Windows retain explicit sockets.
package listen

import (
	"errors"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"sync"
	"syscall"
)

// TCP listens on a numeric wildcard address such as ":11434" or "0.0.0.0:0".
// Addr reports the wildcard socket, including its resolved ephemeral port.
// All accepted connections share one listener; Close releases every socket but
// leaves already handed-off connections alone, like net.TCPListener.Close.
func TCP(address string) (net.Listener, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if host != "" && (ip == nil || !ip.IsUnspecified()) {
		return nil, fmt.Errorf("listen: expected wildcard address, got %q", address)
	}
	var sockets []net.Listener
	success := false
	defer func() {
		if !success {
			for _, ln := range sockets {
				_ = ln.Close()
			}
		}
	}()
	loop4, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return nil, fmt.Errorf("reserve loopback %s: %w", net.JoinHostPort("127.0.0.1", port), err)
	}
	sockets = append(sockets, loop4)
	port = strconv.Itoa(loop4.Addr().(*net.TCPAddr).Port)
	loop6Address := net.JoinHostPort("::1", port)
	loop6, err := net.Listen("tcp6", loop6Address)
	if err != nil && !ipv6Unavailable(err) {
		return nil, fmt.Errorf("reserve loopback %s: %w", loop6Address, err)
	}
	if loop6 != nil {
		sockets = append(sockets, loop6)
	}
	if runtime.GOOS == "linux" {
		for _, socket := range sockets {
			_ = socket.Close()
		}
		sockets = nil
	}
	wildcard, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, fmt.Errorf("reserve wildcard %s: %w", net.JoinHostPort(host, port), err)
	}
	sockets = append(sockets, wildcard)
	if runtime.GOOS == "linux" && loop6 != nil && wildcard.Addr().(*net.TCPAddr).IP.To4() != nil {
		loop6, err = net.Listen("tcp6", loop6Address)
		if err != nil {
			return nil, fmt.Errorf("reserve loopback %s: %w", loop6Address, err)
		}
		sockets = append(sockets, loop6)
	}
	l := &listener{addr: wildcard.Addr(), sockets: sockets, accepted: make(chan acceptResult), done: make(chan struct{})}
	for _, socket := range sockets {
		l.pumps.Go(func() { l.pump(socket) })
	}
	success = true
	return l, nil
}

func ipv6Unavailable(err error) bool {
	return errors.Is(err, syscall.EADDRNOTAVAIL) || errors.Is(err, syscall.EAFNOSUPPORT) || errors.Is(err, syscall.EPROTONOSUPPORT)
}

type acceptResult struct {
	conn net.Conn
	err  error
}
type listener struct {
	addr     net.Addr
	sockets  []net.Listener
	accepted chan acceptResult
	done     chan struct{}
	once     sync.Once
	pumps    sync.WaitGroup
}

func (l *listener) pump(socket net.Listener) {
	for {
		conn, err := socket.Accept()
		select {
		case <-l.done:
			if conn != nil {
				_ = conn.Close()
			}
			return
		case l.accepted <- acceptResult{conn, err}:
		}
		if err != nil {
			if ne, ok := err.(net.Error); !ok || !ne.Temporary() {
				return
			}
		}
	}
}

func (l *listener) Accept() (net.Conn, error) {
	select {
	case <-l.done:
		return nil, net.ErrClosed
	default:
	}
	select {
	case <-l.done:
		return nil, net.ErrClosed
	case result := <-l.accepted:
		if result.err != nil {
			if ne, ok := result.err.(net.Error); !ok || !ne.Temporary() {
				_ = l.Close()
			}
		}
		return result.conn, result.err
	}
}

func (l *listener) Close() error {
	l.once.Do(func() {
		close(l.done)
		for _, socket := range l.sockets {
			_ = socket.Close()
		}
		l.pumps.Wait()
	})
	return nil
}

func (l *listener) Addr() net.Addr { return l.addr }
