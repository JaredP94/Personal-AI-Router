// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http/httptest"
	"testing"
)

func TestBackgroundNodeInfoPollRequiresExplicitHeader(t *testing.T) {
	cases := []struct {
		name       string
		header     string
		remoteAddr string
		want       bool
	}{
		{"explicit background remote", "background", "192.0.2.1:12345", true},
		{"explicit background loopback", "background", "127.0.0.1:12345", true},
		{"explicit foreground remote", "foreground", "192.0.2.1:12345", false},
		{"headerless remote", "", "192.0.2.1:12345", false},
		{"headerless remote IPv6", "", "[2001:db8::1]:12345", false},
		{"headerless loopback", "", "127.0.0.1:12345", false},
		{"unknown header remote", "unknown", "192.0.2.1:12345", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/v1/node-info", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.header != "" {
				req.Header.Set("X-NVPAIR-Poll", tc.header)
			}
			if got := isBackgroundNodeInfoPoll(req); got != tc.want {
				t.Fatalf("isBackgroundNodeInfoPoll = %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("nil request", func(t *testing.T) {
		if isBackgroundNodeInfoPoll(nil) {
			t.Fatal("nil request must not opt into background sampling")
		}
	})
}
