// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "time"

type MacmonStatus string

const (
	MacmonAvailable MacmonStatus = "available"
	MacmonMissing   MacmonStatus = "missing"
	MacmonFailed    MacmonStatus = "failed"
)

type SensorFan struct {
	Name   string `json:"name"`
	RPM    uint32 `json:"rpm"`
	MaxRPM uint32 `json:"maxRpm"`
}

type SensorPower struct {
	SystemW *float64 `json:"systemW,omitempty"`
	CPUW    *float64 `json:"cpuW,omitempty"`
	GPUW    *float64 `json:"gpuW,omitempty"`
	ANEW    *float64 `json:"aneW,omitempty"`
}

// SensorInfo is the /v1/node-info sensors wire object.
type SensorInfo struct {
	Macmon          MacmonStatus `json:"macmon"`
	ThermalPressure string       `json:"thermalPressure,omitempty"`
	MemoryPressure  string       `json:"memoryPressure,omitempty"`
	SwapTotalBytes  *uint64      `json:"swapTotalBytes,omitempty"`
	SwapUsedBytes   *uint64      `json:"swapUsedBytes,omitempty"`
	CPUTempC        *float64     `json:"cpuTempC,omitempty"`
	GPUTempC        *float64     `json:"gpuTempC,omitempty"`
	Fans            *[]SensorFan `json:"fans,omitempty"` // nil = unknown, &[] = no fans
	Power           *SensorPower `json:"power,omitempty"`
	MSSince         *int64       `json:"msSince,omitempty"`
}

// macmonSample is one decoded macmon line (macmon-sourced fields only).
type macmonSample struct {
	CPUTempC, GPUTempC *float64
	Fans               *[]SensorFan
	Power              *SensorPower
	SampledAt          time.Time
}

// directSensors are the readings PAIR takes without macmon.
type directSensors struct {
	ThermalPressure string // "" = unknown
	MemoryPressure  string // "" = unknown
	SwapTotalBytes  *uint64
	SwapUsedBytes   *uint64
}

// sensorState is what the collector publishes; wire turns it into the response object.
type sensorState struct {
	Macmon   MacmonStatus
	Direct   directSensors
	Sample   *macmonSample // latest usable sample, nil when none
	Interval time.Duration // current macmon sampling interval
}

// wire retains measured zeros and drops only stale macmon readings, leaving
// direct readings and macmon status intact. A nil state omits sensors entirely.
func (s *sensorState) wire(now time.Time) *SensorInfo {
	if s == nil {
		return nil
	}
	info := &SensorInfo{
		Macmon:          s.Macmon,
		ThermalPressure: s.Direct.ThermalPressure,
		MemoryPressure:  s.Direct.MemoryPressure,
		SwapTotalBytes:  s.Direct.SwapTotalBytes,
		SwapUsedBytes:   s.Direct.SwapUsedBytes,
	}
	if s.Sample == nil {
		return info
	}
	age := now.Sub(s.Sample.SampledAt)
	if age > 3*s.Interval {
		return info
	}
	info.CPUTempC = s.Sample.CPUTempC
	info.GPUTempC = s.Sample.GPUTempC
	info.Fans = s.Sample.Fans
	if power := s.Sample.Power; power != nil &&
		(power.SystemW != nil || power.CPUW != nil || power.GPUW != nil || power.ANEW != nil) {
		info.Power = power
	}
	msSince := age.Milliseconds()
	info.MSSince = &msSince
	return info
}
