// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package routingmetrics

import "testing"

// These tests are canonical: every proxy depends on this filter, so a change
// here changes the numbers shown in every client. The desktop bridge and
// nvpair-tui previously carried their own copies of exactly this table.

func TestCounts(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		want   bool
	}{
		{"openai chat completions counts", "POST", "/v1/chat/completions", true},
		{"openai completions counts", "POST", "/v1/completions", true},
		{"ollama chat counts", "POST", "/api/chat", true},
		{"ollama generate counts", "POST", "/api/generate", true},
		{"GET is never counted", "GET", "/v1/chat/completions", false},
		{"PUT is never counted", "PUT", "/v1/chat/completions", false},
		{"DELETE is never counted", "DELETE", "/api/generate", false},
		{"HEAD is never counted", "HEAD", "/v1/completions", false},
		{"model list is not counted", "GET", "/api/tags", false},
		{"model list v1 is not counted", "GET", "/v1/models", false},
		{"model show is not counted", "POST", "/api/show", false},
		{"version probe is not counted", "GET", "/api/version", false},
		{"CORS preflight is not counted", "OPTIONS", "/v1/chat/completions", false},
		{"trailing slash is not counted", "POST", "/v1/chat/completions/", false},
		{"empty path is not counted", "POST", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Counts(tc.method, tc.path); got != tc.want {
				t.Errorf("Counts(%q, %q) = %v, want %v", tc.method, tc.path, got, tc.want)
			}
		})
	}
}

func TestCountersZeroValueStartsEmpty(t *testing.T) {
	var c Counters
	if reqs, hits := c.Snapshot(); reqs != 0 || hits != 0 {
		t.Fatalf("fresh counters = %d/%d, want 0/0", reqs, hits)
	}
}

func TestCountersRoutedAccumulates(t *testing.T) {
	var c Counters

	c.Routed(true)
	if reqs, hits := c.Snapshot(); reqs != 1 || hits != 1 {
		t.Fatalf("after affinity hit: %d/%d, want 1/1", reqs, hits)
	}

	c.Routed(false)
	if reqs, hits := c.Snapshot(); reqs != 2 || hits != 1 {
		t.Fatalf("after routed miss: %d/%d, want 2/1", reqs, hits)
	}

	c.Routed(true)
	if reqs, hits := c.Snapshot(); reqs != 3 || hits != 2 {
		t.Fatalf("after second hit: %d/%d, want 3/2", reqs, hits)
	}
}

// Totals are monotonic: no call may lower them, since clients replace rather
// than increment and a regressing total would read as lost traffic.
func TestCountersAreMonotonic(t *testing.T) {
	var c Counters
	prevReqs, prevHits := uint64(0), uint64(0)
	for i := range 64 {
		c.Routed(i%3 != 0)
		reqs, hits := c.Snapshot()
		if reqs < prevReqs || hits < prevHits {
			t.Fatalf("totals regressed at %d: %d/%d after %d/%d", i, reqs, hits, prevReqs, prevHits)
		}
		prevReqs, prevHits = reqs, hits
	}
	if prevReqs != 64 {
		t.Fatalf("requests = %d, want 64", prevReqs)
	}
	if prevHits != 42 {
		// i%3 != 0 over 0..63: multiples of 3 are 0,3,...,63 — 22 of them —
		// so 64-22 = 42 hits.
		t.Fatalf("hits = %d, want 42", prevHits)
	}
}

func TestCountersAreConcurrentSafe(t *testing.T) {
	var c Counters
	const goroutines, per = 8, 100
	done := make(chan struct{})
	for range goroutines {
		go func() {
			for range per {
				c.Routed(true)
			}
			done <- struct{}{}
		}()
	}
	for range goroutines {
		<-done
	}
	if reqs, hits := c.Snapshot(); reqs != goroutines*per || hits != goroutines*per {
		t.Fatalf("after concurrent load: %d/%d, want %d/%d", reqs, hits, goroutines*per, goroutines*per)
	}
}
