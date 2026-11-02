// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"
)

func TestResolveResponseHeaderTimeout(t *testing.T) {
	for _, tc := range []struct {
		name, flag, env string
		want            time.Duration
	}{
		{"default", "", "", defaultProxyResponseTimeout},
		{"environment", "", "10m", 10 * time.Minute},
		{"flag beats environment", "5m", "10m", 5 * time.Minute},
		{"unparseable keeps the default", "soon", "", defaultProxyResponseTimeout},
		{"zero keeps the default", "0s", "", defaultProxyResponseTimeout},
		{"negative keeps the default", "", "-1m", defaultProxyResponseTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(responseHeaderTimeoutEnv, tc.env)
			if got := resolveResponseHeaderTimeout(tc.flag); got != tc.want {
				t.Fatalf("resolveResponseHeaderTimeout(%q) with env %q = %s, want %s", tc.flag, tc.env, got, tc.want)
			}
		})
	}
}

// Transports are built after main resolves the timeout, so they carry it.
func TestTransportsUseTheResolvedTimeout(t *testing.T) {
	old := proxyResponseTimeout
	t.Cleanup(func() { proxyResponseTimeout = old })
	proxyResponseTimeout = 7 * time.Minute
	if got := newProxyTransport(nil).ResponseHeaderTimeout; got != 7*time.Minute {
		t.Fatalf("ResponseHeaderTimeout = %s, want 7m", got)
	}
}
