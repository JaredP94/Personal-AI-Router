// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"errors"
	"fmt"
	"testing"

	"nvpair-shared/jsonrpc"
)

func TestRetryPortBind(t *testing.T) {
	collision := &jsonrpc.RPCError{Code: -32000, Message: "failed to bind port 41001: listen tcp :41001: bind: address already in use"}
	permission := &jsonrpc.RPCError{Code: -32000, Message: "failed to bind port 41001: listen tcp :41001: bind: permission denied"}
	for _, tc := range []struct {
		name      string
		err       error
		failures  int
		wantCalls int
		wantPort  int
		wantErr   bool
	}{
		{"success", nil, 0, 1, 41001, false},
		{"collision then success", collision, 1, 2, 41002, false},
		{"exhausted", collision, 5, 5, 0, true},
		{"permission denied", permission, 1, 1, 0, true},
		{"other failure", errors.New("stream closed"), 1, 1, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			picks, calls := 0, 0
			port, err := retryPortBind(t, func() int { picks++; return 41000 + picks }, func(port int) error {
				calls++
				if calls <= tc.failures {
					if tc.err == collision {
						return &jsonrpc.RPCError{Code: -32000, Message: fmt.Sprintf("failed to bind port %d: listen tcp :%d: bind: address already in use", port, port)}
					}
					return tc.err
				}
				return nil
			})
			if calls != tc.wantCalls || picks != calls || port != tc.wantPort || (err != nil) != tc.wantErr {
				t.Fatalf("port=%d err=%v calls=%d picks=%d; want port=%d error=%v calls=%d", port, err, calls, picks, tc.wantPort, tc.wantErr, tc.wantCalls)
			}
			if tc.wantErr && tc.err != collision && err != tc.err {
				t.Fatalf("lost original error: %v", err)
			}
		})
	}
}

func TestPortBindCollision(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message string
		code    int
		want    bool
	}{
		{"unix", "failed to bind port 41001: listen tcp :41001: bind: address already in use", -32000, true},
		{"windows", "failed to bind port 41001: listen tcp :41001: bind: Only one usage of each socket address (protocol/network address/port) is normally permitted.", -32000, true},
		{"wrong requested port", "failed to bind port 41002: listen tcp :41002: bind: address already in use", -32000, false},
		{"wrong listener port", "failed to bind port 41001: listen tcp :41002: bind: address already in use", -32000, false},
		{"wrong code", "failed to bind port 41001: listen tcp :41001: bind: address already in use", -32602, false},
		{"permission", "failed to bind port 41001: listen tcp :41001: bind: permission denied", -32000, false},
		{"not a bind", "address already in use", -32000, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := &jsonrpc.RPCError{Code: tc.code, Message: tc.message}
			if got := portBindCollision(41001, err); got != tc.want {
				t.Fatalf("portBindCollision = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPortBindCollisionControlListener(t *testing.T) {
	for _, tc := range []struct {
		message string
		want    bool
	}{
		{"listen tcp 0.0.0.0:41001: bind: address already in use", true},
		{"listen tcp 0.0.0.0:41002: bind: address already in use", false},
		{"listen tcp 0.0.0.0:410010: bind: address already in use", false},
		{"listen tcp 0.0.0.0:41001: bind: permission denied", false},
	} {
		if got := portBindCollision(41001, errors.New(tc.message)); got != tc.want {
			t.Errorf("%q: got %v, want %v", tc.message, got, tc.want)
		}
	}
	if portBindCollision(41001, nil) {
		t.Fatal("nil error is not a collision")
	}
}
