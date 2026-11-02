// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"strings"
	"time"
)

// macmonLinePayload deliberately excludes unstable CPU topology and memory fields.
type macmonLinePayload struct {
	Temp *struct {
		CPU *float64 `json:"cpu_temp_avg"`
		GPU *float64 `json:"gpu_temp_avg"`
	} `json:"temp"`
	Fans *[]struct {
		Name   *string  `json:"name"`
		RPM    *float64 `json:"rpm"`
		MaxRPM *float64 `json:"max_rpm"`
	} `json:"fans"`
	SystemW   *float64   `json:"sys_power"`
	CPUW      *float64   `json:"cpu_power"`
	GPUW      *float64   `json:"gpu_power"`
	ANEW      *float64   `json:"ane_power"`
	Timestamp *time.Time `json:"timestamp"`
}

func parseMacmonLine(line []byte) (macmonSample, bool) {
	var payload macmonLinePayload
	if err := json.Unmarshal(line, &payload); err != nil || payload.Timestamp == nil {
		return macmonSample{}, false
	}
	sample := macmonSample{SampledAt: *payload.Timestamp}
	if payload.Temp != nil {
		sample.CPUTempC = payload.Temp.CPU
		sample.GPUTempC = payload.Temp.GPU
	}
	if payload.Fans != nil {
		fans := make([]SensorFan, 0, len(*payload.Fans))
		for _, fan := range *payload.Fans {
			if fan.Name == nil || *fan.Name == "" || fan.RPM == nil || fan.MaxRPM == nil || *fan.RPM < 0 || *fan.MaxRPM < 0 {
				continue
			}
			fans = append(fans, SensorFan{
				Name:   *fan.Name,
				RPM:    uint32(math.Round(*fan.RPM)),
				MaxRPM: uint32(math.Round(*fan.MaxRPM)),
			})
		}
		sample.Fans = &fans
	}
	if payload.SystemW != nil || payload.CPUW != nil || payload.GPUW != nil || payload.ANEW != nil {
		sample.Power = &SensorPower{
			SystemW: payload.SystemW, CPUW: payload.CPUW, GPUW: payload.GPUW, ANEW: payload.ANEW,
		}
	}
	return sample, sample.CPUTempC != nil || sample.GPUTempC != nil || sample.Fans != nil || sample.Power != nil
}

func parseThermalPressure(notifyutilOut string) (string, bool) {
	fields := strings.Fields(notifyutilOut)
	if len(fields) != 2 || fields[0] != "com.apple.system.thermalpressurelevel" {
		return "", false
	}
	switch fields[1] {
	case "0":
		return "nominal", true
	case "1":
		return "moderate", true
	case "2":
		return "heavy", true
	case "3":
		return "trapping", true
	case "4":
		return "sleeping", true
	default:
		return "", false
	}
}

func memoryPressureName(level uint32) (string, bool) {
	switch level {
	case 1:
		return "normal", true
	case 2:
		return "warning", true
	case 4:
		return "critical", true
	default:
		return "", false
	}
}

func parseSwapUsage(raw []byte) (total, used uint64, ok bool) {
	if len(raw) < 24 {
		return 0, 0, false
	}
	return binary.LittleEndian.Uint64(raw[0:8]), binary.LittleEndian.Uint64(raw[16:24]), true
}
