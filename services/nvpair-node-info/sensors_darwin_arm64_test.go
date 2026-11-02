// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && arm64

package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Missing direct collection must not silently produce an empty sensor object.
func TestSensorCollectorLiveDirectReadings(t *testing.T) {
	p := NewSamplingPolicy(func() {})
	c := startSensorCollector(p)
	defer c.stop()
	waitFor(t, func() bool {
		s := c.state()
		return s != nil && s.Direct.MemoryPressure != "" && s.Direct.SwapTotalBytes != nil
	})
	s := c.state()
	switch s.Direct.MemoryPressure {
	case "normal", "warning", "critical":
	default:
		t.Fatalf("invalid memory pressure: %q", s.Direct.MemoryPressure)
	}
	switch s.Direct.ThermalPressure {
	case "", "nominal", "moderate", "heavy", "trapping", "sleeping":
	default:
		t.Fatalf("invalid thermal pressure: %q", s.Direct.ThermalPressure)
	}
	switch s.Macmon {
	case MacmonAvailable, MacmonMissing, MacmonFailed:
	default:
		t.Fatalf("invalid macmon status: %q", s.Macmon)
	}
	if s.Direct.SwapUsedBytes == nil || *s.Direct.SwapUsedBytes > *s.Direct.SwapTotalBytes {
		t.Fatalf("invalid swap readings: %+v", s.Direct)
	}
	if s.Interval != 10*time.Second {
		t.Fatalf("idle sensor interval = %v, want 10s", s.Interval)
	}
}

// A nil collector is used by fake-reader stats tests; repeated/concurrent stops
// must still join both collector goroutines without panicking or hanging.
func TestSensorCollectorStopIsIdempotentAndNilSafe(t *testing.T) {
	var nilC *sensorCollector
	nilC.stop()
	if nilC.state() != nil {
		t.Fatal("nil collector returned sensors")
	}
	c := startSensorCollector(NewSamplingPolicy(func() {}))
	defer c.stop()
	done := make(chan struct{})
	go func() {
		var callers sync.WaitGroup
		for range 4 {
			callers.Add(1)
			go func() {
				defer callers.Done()
				c.stop()
				c.state()
			}()
		}
		callers.Wait()
		c.stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("sensor collector did not stop within 3s")
	}
}

// Without a policy, both macmon and direct collection still have a valid cadence.
func TestSensorCollectorNilPolicy(t *testing.T) {
	c := startSensorCollector(nil)
	defer c.stop()
	if s := c.state(); s == nil || s.Interval != 2*time.Second {
		t.Fatalf("nil-policy state = %+v, want interval 2s", s)
	}
	waitFor(t, func() bool { return c.state().Direct.SwapTotalBytes != nil })
}

// In particular, state must carry Latest's interval: otherwise wire drops even
// a fresh macmon sample as stale. Each caller gets a separate state wrapper.
func TestSensorCollectorStatePreservesFreshMacmonSample(t *testing.T) {
	now := time.Now()
	temp := 42.0
	sample := &macmonSample{CPUTempC: &temp, SampledAt: now.Add(-time.Second)}
	supervisor := newMacmonSupervisor(macmonTestConfig())
	supervisor.status = MacmonAvailable
	supervisor.sample = sample
	supervisor.interval = 2 * time.Second
	supervisor.sampleInterval = 2 * time.Second
	supervisor.started = true
	c := &sensorCollector{supervisor: supervisor}
	c.latest.Store(&directSensors{MemoryPressure: "warning"})
	first, second := c.state(), c.state()
	if first == second {
		t.Fatal("state reused a mutable wrapper")
	}
	wire := first.wire(now)
	if wire.CPUTempC == nil || *wire.CPUTempC != 42 || wire.MemoryPressure != "warning" {
		t.Fatalf("fresh sensor sample lost: %+v", wire)
	}
	first.Direct.MemoryPressure = "critical"
	if c.state().Direct.MemoryPressure != "warning" {
		t.Fatal("mutating returned state changed collector state")
	}
}

// A failed notifyutil call must be unknown rather than a nominal fallback.
func TestSensorCollectorCancelledThermalReadIsUnknown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := readThermalPressure(ctx); got != "" {
		t.Fatalf("cancelled thermal read = %q, want unknown", got)
	}
}

// Only the production entry point starts sensors; a fake stats collector keeps
// its sensor state nil and can be stopped without launching sensor processes.
func TestSensorCollectorFakeStatsStaySensorFree(t *testing.T) {
	c := newDarwinStatsCollector(
		func() darwinCPUTimes { return darwinCPUTimes{} },
		func() (uint64, bool) { return 0, false },
		func(context.Context) (darwinGPUReading, error) { return darwinGPUReading{}, nil },
		time.Hour,
	)
	defer c.Stop()
	if c.sensors != nil || c.PeekSnapshot().Sensors != nil {
		t.Fatal("fake-reader stats collector started sensors")
	}
}
