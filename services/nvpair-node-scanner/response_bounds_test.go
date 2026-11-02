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

// A peer's node-info is parsed only up to nodeInfoResponseLimit. One whose JSON
// value extends past it fails enrichment rather than being buffered.
func TestFetchNodeInfoRejectsAnOversizedAnswer(t *testing.T) {
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
			d := &daemon{http: srv.Client()}
			if _, ok := d.fetchNodeInfo(host, port); ok != tc.want {
				t.Fatalf("fetchNodeInfo ok = %v, want %v", ok, tc.want)
			}
		})
	}
}
