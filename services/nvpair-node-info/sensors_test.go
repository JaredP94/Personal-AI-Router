// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestSensorWireNilStateOmitsSensors(t *testing.T) {
	var state *sensorState
	if got := state.wire(time.Unix(100, 0)); got != nil {
		t.Fatalf("nil state produced sensors: %+v", got)
	}
	body := buildResponseAt(nil, nil, 0, statsSnapshot{}, "h", nil, time.Unix(100, 0))
	if bytes.Contains(body, []byte(`"sensors"`)) {
		t.Fatalf("sensors present: %s", body)
	}
}

func TestSensorWireKeepsMeasuredZerosAndEmptyFans(t *testing.T) {
	zero := uint64(0)
	w := 0.0
	noFans := []SensorFan{}
	now := time.Unix(100, 0)
	state := &sensorState{
		Macmon: MacmonAvailable, Interval: 2 * time.Second,
		Direct: directSensors{
			ThermalPressure: "nominal", MemoryPressure: "normal",
			SwapTotalBytes: &zero, SwapUsedBytes: &zero,
		},
		Sample: &macmonSample{
			CPUTempC: &w, GPUTempC: &w, Fans: &noFans,
			Power:     &SensorPower{SystemW: &w, CPUW: &w, GPUW: &w, ANEW: &w},
			SampledAt: now.Add(-500 * time.Millisecond),
		},
	}
	got := state.wire(now)
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"macmon":"available"`, `"thermalPressure":"nominal"`, `"memoryPressure":"normal"`,
		`"swapTotalBytes":0`, `"swapUsedBytes":0`, `"cpuTempC":0`, `"gpuTempC":0`,
		`"fans":[]`, `"systemW":0`, `"cpuW":0`, `"gpuW":0`, `"aneW":0`, `"msSince":500`,
	} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("missing %s in %s", want, body)
		}
	}

	body = buildResponseAt(nil, nil, 0, statsSnapshot{Sensors: state}, "h", nil, now)
	var response NodeInfoResponse
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(response.Sensors, got) {
		t.Fatalf("response sensors = %+v, want %+v; body: %s", response.Sensors, got, body)
	}
}

func TestSensorWireDropsStaleMacmonReadings(t *testing.T) {
	now := time.Unix(100, 0)
	cpu, gpu, power := 36.0, 35.3, 3.86
	total, used := uint64(1073741824), uint64(6291456)
	fans := []SensorFan{{Name: "fan0", RPM: 0, MaxRPM: 5349}}
	for _, tc := range []struct {
		name     string
		interval time.Duration
		age      time.Duration
		stale    bool
	}{
		{"fresh", 2 * time.Second, 5999 * time.Millisecond, false},
		{"exact boundary", 2 * time.Second, 6 * time.Second, false},
		{"stale", 2 * time.Second, 6001 * time.Millisecond, true},
		{"idle fresh", 10 * time.Second, 29999 * time.Millisecond, false},
		{"idle stale", 10 * time.Second, 30001 * time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := &sensorState{
				Macmon: MacmonAvailable, Interval: tc.interval,
				Direct: directSensors{
					ThermalPressure: "heavy", MemoryPressure: "warning",
					SwapTotalBytes: &total, SwapUsedBytes: &used,
				},
				Sample: &macmonSample{
					CPUTempC: &cpu, GPUTempC: &gpu, Fans: &fans,
					Power: &SensorPower{SystemW: &power}, SampledAt: now.Add(-tc.age),
				},
			}
			want := &SensorInfo{
				Macmon: MacmonAvailable, ThermalPressure: "heavy", MemoryPressure: "warning",
				SwapTotalBytes: &total, SwapUsedBytes: &used,
			}
			if tc.stale {
				want.Macmon = MacmonFailed
			}
			if !tc.stale {
				age := tc.age.Milliseconds()
				want.CPUTempC, want.GPUTempC = &cpu, &gpu
				want.Fans, want.Power, want.MSSince = &fans, &SensorPower{SystemW: &power}, &age
			}
			if got := state.wire(now); !reflect.DeepEqual(got, want) {
				t.Fatalf("wire() = %+v, want %+v", got, want)
			}
			if state.Sample == nil || state.Sample.CPUTempC == nil {
				t.Fatal("wire mutated the retained sample")
			}
		})
	}
}

func TestSensorWireAvailableWithoutSampleIsFailed(t *testing.T) {
	state := &sensorState{Macmon: MacmonAvailable, Direct: directSensors{MemoryPressure: "normal"}}
	got := state.wire(time.Now())
	if got.Macmon != MacmonFailed || got.MemoryPressure != "normal" || got.MSSince != nil {
		t.Fatalf("missing sample wire = %+v, want failed with direct readings", got)
	}
}

func TestSensorWireClampsNegativeAge(t *testing.T) {
	now := time.Now()
	cpu := 36.0
	state := &sensorState{Macmon: MacmonAvailable, Interval: 2 * time.Second,
		Sample: &macmonSample{CPUTempC: &cpu, SampledAt: now.Add(time.Hour)}}
	got := state.wire(now)
	if got.MSSince == nil || *got.MSSince != 0 || got.CPUTempC == nil || got.Macmon != MacmonAvailable {
		t.Fatalf("future sample wire = %+v, want available with zero age", got)
	}
}

func TestSensorWireMissingMacmonHasDirectOnly(t *testing.T) {
	state := &sensorState{Macmon: MacmonMissing, Direct: directSensors{MemoryPressure: "normal"}}
	body, err := json.Marshal(state.wire(time.Unix(100, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"macmon":"missing","memoryPressure":"normal"}`; string(body) != want {
		t.Fatalf("wire JSON = %s, want %s", body, want)
	}
}

func TestSensorWireOmitsUnknownReadings(t *testing.T) {
	now := time.Unix(100, 0)
	cpu := 36.0
	state := &sensorState{
		Macmon: MacmonFailed, Interval: 2 * time.Second,
		Sample: &macmonSample{CPUTempC: &cpu, Power: &SensorPower{}, SampledAt: now},
	}
	body, err := json.Marshal(state.wire(now))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"macmon":"failed","cpuTempC":36,"msSince":0}`; string(body) != want {
		t.Fatalf("wire JSON = %s, want %s", body, want)
	}
}

func TestSensorWireOmitsUnknownPowerFields(t *testing.T) {
	now := time.Unix(100, 0)
	zero := 0.0
	state := &sensorState{
		Macmon: MacmonAvailable, Interval: 2 * time.Second,
		Sample: &macmonSample{Power: &SensorPower{ANEW: &zero}, SampledAt: now},
	}
	body, err := json.Marshal(state.wire(now))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"macmon":"available","power":{"aneW":0},"msSince":0}`; string(body) != want {
		t.Fatalf("wire JSON = %s, want %s", body, want)
	}
}
