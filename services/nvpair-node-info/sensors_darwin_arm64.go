// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && arm64

package main

import (
	"context"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// Direct readings and supervisor samples are immutable once published.
type sensorCollector struct {
	latest     atomic.Pointer[directSensors]
	supervisor *macmonSupervisor
	policy     *SamplingPolicy
	cancel     context.CancelFunc
	done       sync.WaitGroup
	stopOnce   sync.Once
}

func startSensorCollector(policy *SamplingPolicy) *sensorCollector {
	ctx, cancel := context.WithCancel(context.Background())
	c := &sensorCollector{policy: policy, cancel: cancel}
	c.supervisor = newMacmonSupervisor(macmonConfig{
		Paths:      []string{"/opt/homebrew/bin/macmon", "/usr/local/bin/macmon"},
		Interval:   func() time.Duration { return c.interval(time.Now()) },
		BackoffMin: time.Second,
		BackoffMax: 30 * time.Second,
		CheckEvery: time.Second,
	})
	c.done.Add(2)
	go c.runDirect(ctx)
	go func() {
		defer c.done.Done()
		c.supervisor.Run(ctx)
	}()
	return c
}

func (c *sensorCollector) interval(now time.Time) time.Duration {
	if c.policy != nil {
		return c.policy.Interval(now)
	}
	return statsTickInterval
}

func (c *sensorCollector) runDirect(ctx context.Context) {
	defer c.done.Done()
	ticker := time.NewTicker(statsTickInterval)
	defer ticker.Stop()
	var lastThermal time.Time
	var thermal string
	for {
		if ctx.Err() != nil {
			return
		}
		now := time.Now()
		direct := directSensors{MemoryPressure: readMemoryPressure()}
		direct.SwapTotalBytes, direct.SwapUsedBytes = readSwap()
		if lastThermal.IsZero() || now.Sub(lastThermal) >= c.interval(now) {
			thermal = readThermalPressure(ctx)
			lastThermal = now
		}
		direct.ThermalPressure = thermal
		c.latest.Store(&direct)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *sensorCollector) state() *sensorState {
	if c == nil {
		return nil
	}
	status, sample, interval := c.supervisor.Latest()
	state := &sensorState{Macmon: status, Sample: sample, Interval: interval}
	if direct := c.latest.Load(); direct != nil {
		state.Direct = *direct
	}
	return state
}

func (c *sensorCollector) stop() {
	if c == nil {
		return
	}
	c.stopOnce.Do(func() {
		c.cancel()
		// Run does not return until the macmon child is killed and reaped.
		c.done.Wait()
	})
}

func readMemoryPressure() string {
	level, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level")
	if err != nil {
		return ""
	}
	name, _ := memoryPressureName(level)
	return name
}

func readSwap() (*uint64, *uint64) {
	raw, err := unix.SysctlRaw("vm.swapusage")
	if err != nil {
		return nil, nil
	}
	total, used, ok := parseSwapUsage(raw)
	if !ok {
		return nil, nil
	}
	return &total, &used
}

func readThermalPressure(ctx context.Context) string {
	collectCtx, cancel := context.WithTimeout(ctx, CollectionTimeout)
	defer cancel()
	out, err := exec.CommandContext(collectCtx, "/usr/bin/notifyutil", "-g", "com.apple.system.thermalpressurelevel").Output()
	if err != nil {
		return ""
	}
	pressure, _ := parseThermalPressure(string(out))
	return pressure
}
