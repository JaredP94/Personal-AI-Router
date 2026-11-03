// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"
)

func TestResolveFirstBodyTimeout(t *testing.T) {
	for _, tc := range []struct {
		name, flag, env string
		want            time.Duration
	}{
		{"default disabled", "", "", 0},
		{"environment opt-in", "", "10m", 10 * time.Minute},
		{"flag opt-in", "7m", "", 7 * time.Minute},
		{"flag beats environment", "5m", "10m", 5 * time.Minute},
		{"explicit zero disables environment", "0s", "10m", 0},
		{"environment zero disables", "", "0", 0},
		{"invalid flag disables", "soon", "10m", 0},
		{"invalid environment disables", "", "soon", 0},
		{"negative flag disables", "-1s", "10m", 0},
		{"negative environment disables", "", "-1m", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(firstBodyTimeoutEnv, tc.env)
			if got := resolveFirstBodyTimeout(tc.flag); got != tc.want {
				t.Fatalf("first-body timeout = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestFirstBodyTimeoutDoesNotChangeHeaderDeadline(t *testing.T) {
	old := proxyFirstBodyTimeout
	t.Cleanup(func() { proxyFirstBodyTimeout = old })
	proxyFirstBodyTimeout = 7 * time.Minute
	if got := newProxyTransport(nil).ResponseHeaderTimeout; got != proxyResponseTimeout {
		t.Fatalf("header timeout = %s, want unchanged %s", got, proxyResponseTimeout)
	}
}
