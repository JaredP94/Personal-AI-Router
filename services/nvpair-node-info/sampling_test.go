// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"container/heap"
	"testing"
	"time"
)

// TestPolicyTable covers every combination of interactive lease, scheduling lease,
// localExecution demand, and their expiries.
func TestPolicyTable(t *testing.T) {
	epoch := time.Unix(1000000, 0)

	cases := []struct {
		name           string
		setup          func(p *SamplingPolicy, now time.Time)
		evalAt         time.Duration
		wantInterval   time.Duration
		wantMode       SamplingMode
	}{
		{
			name:         "clean idle: no leases, no demand",
			setup:        func(p *SamplingPolicy, now time.Time) {},
			evalAt:       0,
			wantInterval: IdleInterval,
			wantMode:     ModeIdle,
		},
		{
			name: "interactive lease active: interval 2s",
			setup: func(p *SamplingPolicy, now time.Time) {
				p.RenewInteractive(now)
			},
			evalAt:       1 * time.Second,
			wantInterval: InteractiveInterval,
			wantMode:     ModeInteractive,
		},
		{
			name: "local execution active: interval 2s",
			setup: func(p *SamplingPolicy, now time.Time) {
				p.SetLocalExecution(true, now)
			},
			evalAt:       1 * time.Second,
			wantInterval: InteractiveInterval,
			wantMode:     ModeInteractive,
		},
		{
			name: "scheduling lease active: interval 5s",
			setup: func(p *SamplingPolicy, now time.Time) {
				p.RenewScheduling(now)
			},
			evalAt:       2 * time.Second,
			wantInterval: SchedulingInterval,
			wantMode:     ModeScheduling,
		},
		{
			name: "both leases active: interactive takes precedence (2s)",
			setup: func(p *SamplingPolicy, now time.Time) {
				p.RenewScheduling(now)
				p.RenewInteractive(now)
			},
			evalAt:       1 * time.Second,
			wantInterval: InteractiveInterval,
			wantMode:     ModeInteractive,
		},
		{
			name: "interactive lease expires at 6s: falls back to scheduling (5s)",
			setup: func(p *SamplingPolicy, now time.Time) {
				p.RenewInteractive(now)
				p.RenewScheduling(now.Add(2 * time.Second)) // expires at now + 8s
			},
			evalAt:       6500 * time.Millisecond,
			wantInterval: SchedulingInterval,
			wantMode:     ModeScheduling,
		},
		{
			name: "interactive lease expires at 6s without scheduling: falls back to idle (10s)",
			setup: func(p *SamplingPolicy, now time.Time) {
				p.RenewInteractive(now)
			},
			evalAt:       6500 * time.Millisecond,
			wantInterval: IdleInterval,
			wantMode:     ModeIdle,
		},
		{
			name: "scheduling lease expires at 6s: falls back to idle (10s)",
			setup: func(p *SamplingPolicy, now time.Time) {
				p.RenewScheduling(now)
			},
			evalAt:       6500 * time.Millisecond,
			wantInterval: IdleInterval,
			wantMode:     ModeIdle,
		},
		{
			name: "local execution persists past lease TTL (2s)",
			setup: func(p *SamplingPolicy, now time.Time) {
				p.SetLocalExecution(true, now)
			},
			evalAt:       30 * time.Second,
			wantInterval: InteractiveInterval,
			wantMode:     ModeInteractive,
		},
		{
			name: "local execution cleared: falls back to idle (10s)",
			setup: func(p *SamplingPolicy, now time.Time) {
				p.SetLocalExecution(true, now)
				p.SetLocalExecution(false, now.Add(time.Second))
			},
			evalAt:       2 * time.Second,
			wantInterval: IdleInterval,
			wantMode:     ModeIdle,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewSamplingPolicy(nil)
			tc.setup(p, epoch)
			now := epoch.Add(tc.evalAt)
			if got := p.Interval(now); got != tc.wantInterval {
				t.Fatalf("Interval() = %v, want %v", got, tc.wantInterval)
			}
			if got := p.Mode(now); got != tc.wantMode {
				t.Fatalf("Mode() = %v, want %v", got, tc.wantMode)
			}
		})
	}
}

// TestWakeOnRaise verifies that wake triggers if and only if the mode is strictly raised.
func TestWakeOnRaise(t *testing.T) {
	epoch := time.Unix(1000000, 0)
	wakes := 0
	wakeFn := func() {
		wakes++
	}
	p := NewSamplingPolicy(wakeFn)

	// 1. Idle -> renew scheduling: raises mode (Idle -> Scheduling), must wake.
	if raised := p.RenewScheduling(epoch); !raised || wakes != 1 {
		t.Fatalf("Idle -> Scheduling: raised=%v, wakes=%d; want true/1", raised, wakes)
	}

	// 2. Scheduling -> renew scheduling: same mode, must NOT wake.
	if raised := p.RenewScheduling(epoch.Add(time.Second)); raised || wakes != 1 {
		t.Fatalf("Scheduling -> Scheduling: raised=%v, wakes=%d; want false/1", raised, wakes)
	}

	// 3. Scheduling -> renew interactive: raises mode (Scheduling -> Interactive), must wake.
	if raised := p.RenewInteractive(epoch.Add(2 * time.Second)); !raised || wakes != 2 {
		t.Fatalf("Scheduling -> Interactive: raised=%v, wakes=%d; want true/2", raised, wakes)
	}

	// 4. Interactive -> renew interactive: same mode, must NOT wake.
	if raised := p.RenewInteractive(epoch.Add(3 * time.Second)); raised || wakes != 2 {
		t.Fatalf("Interactive -> Interactive: raised=%v, wakes=%d; want false/2", raised, wakes)
	}

	// 5. Interactive -> renew scheduling: lower than current interactive, must NOT wake.
	if raised := p.RenewScheduling(epoch.Add(4 * time.Second)); raised || wakes != 2 {
		t.Fatalf("Interactive -> Scheduling: raised=%v, wakes=%d; want false/2", raised, wakes)
	}

	// 6. Fast forward until all leases expire -> now Idle.
	afterExpiry := epoch.Add(20 * time.Second)
	if m := p.Mode(afterExpiry); m != ModeIdle {
		t.Fatalf("expected ModeIdle after expiry, got %v", m)
	}

	// 7. Idle -> localExecution=true: raises mode (Idle -> Interactive), must wake.
	if raised := p.SetLocalExecution(true, afterExpiry); !raised || wakes != 3 {
		t.Fatalf("Idle -> localExecution: raised=%v, wakes=%d; want true/3", raised, wakes)
	}

	// 8. localExecution=true -> localExecution=false: lowers mode, must NOT wake.
	if raised := p.SetLocalExecution(false, afterExpiry.Add(time.Second)); raised || wakes != 3 {
		t.Fatalf("localExecution=false: raised=%v, wakes=%d; want false/3", raised, wakes)
	}
}

// TestPollIntentClassification verifies X-NVPAIR-Poll header parsing.
func TestPollIntentClassification(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{"foreground", PollIntentForeground},
		{"scheduling", PollIntentScheduling},
		{"background", PollIntentBackground},
		{"", PollIntentForeground},        // missing header behaves as foreground
		{"unknown", PollIntentForeground},   // unknown header behaves as foreground
		{"FOREGROUND", PollIntentForeground},// unknown/cased behaves as foreground
	}
	for _, tc := range cases {
		if got := parsePollIntent(tc.header); got != tc.want {
			t.Errorf("parsePollIntent(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}

// Virtual clock simulation for TestSchedulingFreshnessBudget
type simEventType int

const (
	eventCollectStart simEventType = iota
	eventCollectFinish
	eventScannerPoll
)

type simEvent struct {
	at   time.Duration
	kind simEventType
}

type simEventQueue []simEvent

func (q simEventQueue) Len() int           { return len(q) }
func (q simEventQueue) Less(i, j int) bool { return q[i].at < q[j].at }
func (q simEventQueue) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }
func (q *simEventQueue) Push(x any)        { *q = append(*q, x.(simEvent)) }
func (q *simEventQueue) Pop() any {
	old := *q
	n := len(old)
	item := old[n-1]
	*q = old[0 : n-1]
	return item
}

// TestSchedulingFreshnessBudget: virtual clock, scheduling lease kept alive by reads
// every 2.25 s with 0.5 s request latency, collection taking up to 1 s, swept over
// every phase offset in 50 ms steps for 60 s -> the age delivered to the scheduler
// is always <= 10 s.
func TestSchedulingFreshnessBudget(t *testing.T) {
	const (
		readInterval   = 2250 * time.Millisecond // 2.25 s
		requestLatency = 500 * time.Millisecond  // 0.5 s
		collectTime    = 1000 * time.Millisecond // up to 1 s
		testDuration   = 60 * time.Second
		freshnessLimit = 10 * time.Second
	)

	var maxDeliveredAgeOverall time.Duration

	// Sweep phase offsets from 0 to 2250 ms in 50 ms steps
	for phase := 0 * time.Millisecond; phase < readInterval; phase += 50 * time.Millisecond {
		var (
			now            time.Duration
			lastSampledAt  time.Duration
			collecting     bool
			collectPending bool
			events         simEventQueue
		)

		epoch := time.Unix(1000000, 0)
		var wakeSignal func()
		policy := NewSamplingPolicy(func() {
			if wakeSignal != nil {
				wakeSignal()
			}
		})

		wakeSignal = func() {
			if !collecting && collectPending {
				// Reschedule collection to start immediately
				for i := range events {
					if events[i].kind == eventCollectStart {
						events[i].at = now
						heap.Init(&events)
						break
					}
				}
			}
		}

		heap.Init(&events)

		// Initial sample collected at t=0
		lastSampledAt = 0
		collectPending = true
		heap.Push(&events, simEvent{at: policy.Interval(epoch), kind: eventCollectStart})

		// Schedule first scanner poll at phase offset
		heap.Push(&events, simEvent{at: phase, kind: eventScannerPoll})

		for events.Len() > 0 {
			ev := heap.Pop(&events).(simEvent)
			now = ev.at
			if now > testDuration+readInterval {
				break
			}

			switch ev.kind {
			case eventScannerPoll:
				// Request arrives at node-info, renews scheduling lease
				policy.RenewScheduling(epoch.Add(now))

				// node-info computes msSince
				msSince := now - lastSampledAt
				// response travels over network to scheduler with latency
				deliveredAge := msSince + requestLatency

				if deliveredAge > freshnessLimit {
					t.Fatalf("phase %v at %v: delivered age %v exceeded %v limit (sampledAt=%v, msSince=%v)",
						phase, now, deliveredAge, freshnessLimit, lastSampledAt, msSince)
				}
				if deliveredAge > maxDeliveredAgeOverall {
					maxDeliveredAgeOverall = deliveredAge
				}

				// Schedule next poll
				if now+readInterval <= testDuration+readInterval {
					heap.Push(&events, simEvent{at: now + readInterval, kind: eventScannerPoll})
				}

			case eventCollectStart:
				collectPending = false
				collecting = true
				heap.Push(&events, simEvent{at: now + collectTime, kind: eventCollectFinish})

			case eventCollectFinish:
				collecting = false
				if collectTime <= CollectionTimeout {
					lastSampledAt = now
				}
				interval := policy.Interval(epoch.Add(now))
				collectPending = true
				heap.Push(&events, simEvent{at: now + interval, kind: eventCollectStart})
			}
		}
	}

	t.Logf("Max delivered age across 60s sweep: %v (budget limit %v)", maxDeliveredAgeOverall, freshnessLimit)
	if maxDeliveredAgeOverall > freshnessLimit {
		t.Fatalf("max delivered age %v > %v", maxDeliveredAgeOverall, freshnessLimit)
	}
}

// TestFreshnessTimeoutCompanion verifies that with collection always timing out,
// delivered age grows past 10 s (reported stale, never reset).
func TestFreshnessTimeoutCompanion(t *testing.T) {
	const (
		readInterval   = 2250 * time.Millisecond
		requestLatency = 500 * time.Millisecond
		collectTime    = 1500 * time.Millisecond // > CollectionTimeout (1s), so times out!
		testDuration   = 20 * time.Second
	)

	epoch := time.Unix(1000000, 0)
	policy := NewSamplingPolicy(nil)

	var (
		now           time.Duration
		lastSampledAt time.Duration = 0 // Initial sample at t=0
		events        simEventQueue
		grewPast10s   bool
		lastAge       time.Duration
	)
	heap.Init(&events)

	// First collection scheduled at 5s
	heap.Push(&events, simEvent{at: policy.Interval(epoch), kind: eventCollectStart})
	// Scanner polls start at 0
	heap.Push(&events, simEvent{at: 0, kind: eventScannerPoll})

	for events.Len() > 0 {
		ev := heap.Pop(&events).(simEvent)
		now = ev.at
		if now > testDuration {
			break
		}

		switch ev.kind {
		case eventScannerPoll:
			policy.RenewScheduling(epoch.Add(now))
			msSince := now - lastSampledAt
			deliveredAge := msSince + requestLatency

			// Verify age is monotonically increasing (never reset or under-reported)
			if deliveredAge < lastAge {
				t.Fatalf("at %v: delivered age regressed from %v to %v (reset detected!)", now, lastAge, deliveredAge)
			}
			lastAge = deliveredAge

			if deliveredAge > 10*time.Second {
				grewPast10s = true
			}

			if now+readInterval <= testDuration {
				heap.Push(&events, simEvent{at: now + readInterval, kind: eventScannerPoll})
			}

		case eventCollectStart:
			heap.Push(&events, simEvent{at: now + collectTime, kind: eventCollectFinish})

		case eventCollectFinish:
			// Collection timed out (1.5s > 1s CollectionTimeout) -> publishes NOTHING!
			// lastSampledAt is NOT updated!
			interval := policy.Interval(epoch.Add(now))
			heap.Push(&events, simEvent{at: now + interval, kind: eventCollectStart})
		}
	}

	if !grewPast10s {
		t.Fatalf("expected delivered age to grow past 10s when collection times out; final age was %v", lastAge)
	}
}
