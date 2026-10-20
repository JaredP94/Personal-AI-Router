// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"testing"
)

// TestParseProcStat pins the /proc/stat aggregate-line parse. The collector
// derives CPU % from the delta of two of these samples, so a regression here
// (e.g. picking up a per-core "cpuN" line, or miscounting idle) would make
// every Linux node report a wrong or zero utilization.
func TestParseProcStat(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantIdle  uint64
		wantTotal uint64
		wantValid bool
	}{
		{
			name:      "typical aggregate line, idle = idle+iowait",
			in:        "cpu  100 0 50 800 40 0 10 0 0 0\ncpu0 50 0 25 400 20 0 5 0 0 0\n",
			wantIdle:  840, // idle 800 + iowait 40
			wantTotal: 1000,
			wantValid: true,
		},
		{
			name:      "ignores cpuN lines, only aggregate counts",
			in:        "cpu0 50 0 25 400 20 0 5 0 0 0\ncpu  10 0 10 70 10 0 0 0 0 0\n",
			wantIdle:  80, // 70 + 10
			wantTotal: 100,
			wantValid: true,
		},
		{
			name:      "no aggregate cpu line",
			in:        "intr 12345\nctxt 6789\n",
			wantValid: false,
		},
		{
			name:      "too few fields",
			in:        "cpu 1 2\n",
			wantValid: false,
		},
		{
			name:      "all-zero totals are invalid",
			in:        "cpu  0 0 0 0 0 0 0 0 0 0\n",
			wantValid: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseProcStat(c.in)
			if got.valid != c.wantValid {
				t.Fatalf("valid = %v, want %v", got.valid, c.wantValid)
			}
			if !c.wantValid {
				return
			}
			if got.idle != c.wantIdle || got.total != c.wantTotal {
				t.Fatalf("idle/total = %d/%d, want %d/%d",
					got.idle, got.total, c.wantIdle, c.wantTotal)
			}
		})
	}
}

// TestCPUUtilization exercises the delta math and every guard: invalid
// samples, counter resets, zero elapsed time, and rounding.
func TestCPUUtilization(t *testing.T) {
	cases := []struct {
		name string
		prev cpuTimes
		cur  cpuTimes
		want uint32
	}{
		{
			name: "50 percent busy",
			prev: cpuTimes{idle: 100, total: 200, valid: true},
			cur:  cpuTimes{idle: 200, total: 400, valid: true},
			want: 50,
		},
		{
			name: "fully busy",
			prev: cpuTimes{idle: 100, total: 200, valid: true},
			cur:  cpuTimes{idle: 100, total: 300, valid: true},
			want: 100,
		},
		{
			name: "fully idle",
			prev: cpuTimes{idle: 100, total: 200, valid: true},
			cur:  cpuTimes{idle: 200, total: 300, valid: true},
			want: 0,
		},
		{
			name: "rounds half up",
			prev: cpuTimes{idle: 0, total: 0, valid: true},
			cur:  cpuTimes{idle: 425, total: 1000, valid: true}, // 57.5% busy
			want: 58,
		},
		{
			name: "invalid prev -> 0",
			prev: cpuTimes{},
			cur:  cpuTimes{idle: 1, total: 2, valid: true},
			want: 0,
		},
		{
			name: "counter reset (total went backwards) -> 0",
			prev: cpuTimes{idle: 100, total: 500, valid: true},
			cur:  cpuTimes{idle: 50, total: 200, valid: true},
			want: 0,
		},
		{
			name: "no elapsed jiffies -> 0",
			prev: cpuTimes{idle: 100, total: 200, valid: true},
			cur:  cpuTimes{idle: 100, total: 200, valid: true},
			want: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cpuUtilization(c.prev, c.cur); got != c.want {
				t.Fatalf("cpuUtilization() = %d, want %d", got, c.want)
			}
		})
	}
}

// TestInitialMemorySnapshot verifies that startup publishes a usable memory
// sample before the first ticker event while preserving omission semantics
// when /proc/meminfo cannot be read.
func TestInitialMemorySnapshot(t *testing.T) {
	cases := []struct {
		name      string
		readUsed  func() (uint64, bool)
		wantUsed  uint64
		wantValid bool
	}{
		{
			name: "successful startup read",
			readUsed: func() (uint64, bool) {
				return 48 << 30, true
			},
			wantUsed:  48 << 30,
			wantValid: true,
		},
		{
			name:      "measured zero startup read",
			readUsed:  func() (uint64, bool) { return 0, true },
			wantValid: true,
		},
		{
			name: "failed startup read remains unknown",
			readUsed: func() (uint64, bool) {
				return 123, false
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			snap := initialMemorySnapshot(c.readUsed)
			if snap.MemUsedValid != c.wantValid {
				t.Fatalf("MemUsedValid = %v, want %v", snap.MemUsedValid, c.wantValid)
			}
			if snap.MemUsedBytes != c.wantUsed {
				t.Fatalf("MemUsedBytes = %d, want %d", snap.MemUsedBytes, c.wantUsed)
			}
		})
	}
}

// TestParseMeminfoUsed pins the MemTotal-MemAvailable computation, the
// MemFree fallback for pre-3.14 kernels, the kB->bytes conversion, and the
// failure cases (missing total, underflow).
func TestParseMeminfoUsed(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantUsed uint64
		wantOK   bool
	}{
		{
			name:     "MemAvailable present",
			in:       "MemTotal:       1000 kB\nMemFree:         200 kB\nMemAvailable:    400 kB\n",
			wantUsed: 600 * 1024, // (1000 - 400) kB
			wantOK:   true,
		},
		{
			name:     "MemAvailable absent, falls back to MemFree",
			in:       "MemTotal:       1000 kB\nMemFree:         200 kB\nBuffers:          50 kB\n",
			wantUsed: 800 * 1024, // (1000 - 200) kB
			wantOK:   true,
		},
		{
			name:   "missing MemTotal",
			in:     "MemFree:         200 kB\nMemAvailable:    400 kB\n",
			wantOK: false,
		},
		{
			name:   "available exceeds total and no usable free -> fail",
			in:     "MemTotal:        100 kB\nMemAvailable:    200 kB\n",
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			used, ok := parseMeminfoUsed(c.in)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && used != c.wantUsed {
				t.Fatalf("used = %d, want %d", used, c.wantUsed)
			}
		})
	}
}
