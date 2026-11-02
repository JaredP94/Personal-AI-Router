// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// A manual node's node-info is parsed only up to nodeInfoResponseLimit. One
// whose JSON value extends past it follows the probe's failure path.
func TestProbeNodeInfoRejectsAnOversizedAnswer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		padding int
		want    bool
	}{
		{"within the limit", 16, true},
		{"past the limit", nodeInfoResponseLimit, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w, `{"hostUuid":"peer-uuid","hostname":"%s"}`, strings.Repeat("x", tc.padding))
			}))
			defer srv.Close()
			host, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			port, err := strconv.Atoi(portStr)
			if err != nil {
				t.Fatal(err)
			}
			m, _, _ := newTestManager()
			if up, _ := m.probeNodeInfo(srv.Client(), "http", host, port); up != tc.want {
				t.Fatalf("probeNodeInfo up = %v, want %v", up, tc.want)
			}
		})
	}
}
