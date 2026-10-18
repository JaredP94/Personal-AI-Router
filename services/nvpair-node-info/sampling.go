// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"sync"
	"time"
)

const (
	InteractiveInterval = 2 * time.Second
	SchedulingInterval  = 5 * time.Second
	IdleInterval        = 10 * time.Second
	LeaseTTL            = 6 * time.Second
	CollectionTimeout   = 1 * time.Second
)

const (
	PollIntentForeground = "foreground"
	PollIntentScheduling = "scheduling"
	PollIntentBackground = "background"
)

// parsePollIntent parses the X-NVPAIR-Poll header.
// - "foreground" renews the interactive lease;
// - "scheduling" renews the scheduling lease;
// - "background" renews nothing and peeks;
// - missing or unknown header behaves as "foreground".
func parsePollIntent(header string) string {
	switch header {
	case PollIntentBackground:
		return PollIntentBackground
	case PollIntentScheduling:
		return PollIntentScheduling
	case PollIntentForeground:
		return PollIntentForeground
	default:
		return PollIntentForeground
	}
}

type SamplingMode int

const (
	ModeIdle SamplingMode = iota
	ModeScheduling
	ModeInteractive
)

// SamplingPolicy determines the sampling interval based on active client leases
// and local execution demand.
type SamplingPolicy struct {
	mu                  sync.Mutex
	interactiveInterval time.Duration
	schedulingInterval  time.Duration
	idleInterval        time.Duration
	leaseTTL            time.Duration

	interactiveUntil time.Time
	schedulingUntil  time.Time
	localExecution   bool
	wake             func()
}

// NewSamplingPolicy creates a new SamplingPolicy with the standard intervals.
// wake is called whenever a lease renewal or demand update raises the mode.
func NewSamplingPolicy(wake func()) *SamplingPolicy {
	return &SamplingPolicy{
		interactiveInterval: InteractiveInterval,
		schedulingInterval:  SchedulingInterval,
		idleInterval:        IdleInterval,
		leaseTTL:            LeaseTTL,
		wake:                wake,
	}
}

// Mode returns the effective sampling mode at now:
// - Interactive if interactive lease or localExecution is active;
// - Scheduling if scheduling lease is active;
// - Idle otherwise.
func (p *SamplingPolicy) Mode(now time.Time) SamplingMode {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.modeLocked(now)
}

func (p *SamplingPolicy) modeLocked(now time.Time) SamplingMode {
	if p.localExecution || now.Before(p.interactiveUntil) {
		return ModeInteractive
	}
	if now.Before(p.schedulingUntil) {
		return ModeScheduling
	}
	return ModeIdle
}

// Interval returns the sampling cadence for now:
// interactive lease or localExecution -> 2s; else scheduling lease -> 5s; else 10s.
func (p *SamplingPolicy) Interval(now time.Time) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.modeLocked(now)
	switch m {
	case ModeInteractive:
		if p.interactiveInterval > 0 {
			return p.interactiveInterval
		}
		return InteractiveInterval
	case ModeScheduling:
		if p.schedulingInterval > 0 {
			return p.schedulingInterval
		}
		return SchedulingInterval
	default:
		if p.idleInterval > 0 {
			return p.idleInterval
		}
		return IdleInterval
	}
}

func (p *SamplingPolicy) ttlLocked() time.Duration {
	if p.leaseTTL > 0 {
		return p.leaseTTL
	}
	return LeaseTTL
}

// RenewInteractive renews the interactive lease until now + LeaseTTL.
// Returns whether the mode was raised, and wakes the collector if so.
func (p *SamplingPolicy) RenewInteractive(now time.Time) bool {
	p.mu.Lock()
	prevMode := p.modeLocked(now)
	until := now.Add(p.ttlLocked())
	if until.After(p.interactiveUntil) {
		p.interactiveUntil = until
	}
	newMode := p.modeLocked(now)
	raised := newMode > prevMode
	wake := p.wake
	p.mu.Unlock()

	if raised && wake != nil {
		wake()
	}
	return raised
}

// RenewScheduling renews the scheduling lease until now + LeaseTTL.
// Returns whether the mode was raised, and wakes the collector if so.
func (p *SamplingPolicy) RenewScheduling(now time.Time) bool {
	p.mu.Lock()
	prevMode := p.modeLocked(now)
	until := now.Add(p.ttlLocked())
	if until.After(p.schedulingUntil) {
		p.schedulingUntil = until
	}
	newMode := p.modeLocked(now)
	raised := newMode > prevMode
	wake := p.wake
	p.mu.Unlock()

	if raised && wake != nil {
		wake()
	}
	return raised
}

// SetLocalExecution sets whether a local workload is executing on this node.
// Returns whether the mode was raised, and wakes the collector if so.
func (p *SamplingPolicy) SetLocalExecution(active bool, now time.Time) bool {
	p.mu.Lock()
	prevMode := p.modeLocked(now)
	p.localExecution = active
	newMode := p.modeLocked(now)
	raised := newMode > prevMode
	wake := p.wake
	p.mu.Unlock()

	if raised && wake != nil {
		wake()
	}
	return raised
}
