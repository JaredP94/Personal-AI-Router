// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"reflect"
	"testing"
	"time"
)

// macmon 0.10.0, Apple M5 Max, macOS 27.2.
// Captured line with only cpu_tiers, ecpu_cores, and pcpu_cores removed.
const macmonM5MaxLine = `{"all_power":3.4984476566314697,"ane_power":0.0,"cpu_active_ratio":0.1137610673904419,"cpu_power":3.1557815074920654,"cpu_scaled_ratio":0.07150898873806,"cpu_usage_pct":0.07150898873806,"ecpu_active_ratio":0.12531398236751556,"ecpu_freq_mhz":2276,"ecpu_scaled_ratio":0.07273218780755997,"ecpu_usage":[2276,0.07273218780755997],"fans":[{"max_rpm":5349,"name":"fan0","rpm":0},{"max_rpm":5777,"name":"fan1","rpm":0}],"gpu_active_ratio":0.17544320225715637,"gpu_freq_mhz":338,"gpu_power":0.3426660895347595,"gpu_ram_power":0.0,"gpu_scaled_ratio":0.03660481795668602,"gpu_usage":[338,0.03660481795668602],"memory":{"ram_total":137438953472,"ram_usage":42524704768,"swap_total":1073741824,"swap_usage":6291456},"pcpu_active_ratio":0.09065523743629456,"pcpu_freq_mhz":3658,"pcpu_scaled_ratio":0.06906260550022125,"pcpu_usage":[3658,0.06906260550022125],"ram_power":0.0,"sys_power":11.486136436462402,"temp":{"cpu_temp_avg":40.40830612182617,"gpu_temp_avg":40.07328796386719},"timestamp":"2026-11-02T06:29:04.730324+00:00"}`

func TestParseMacmonLineRealM5Max(t *testing.T) {
	s, ok := parseMacmonLine([]byte(macmonM5MaxLine))
	if !ok {
		t.Fatal("real macmon line rejected")
	}
	cpu, gpu := 40.40830612182617, 40.07328796386719
	systemW, cpuW, gpuW, aneW := 11.486136436462402, 3.1557815074920654, 0.3426660895347595, 0.0
	fans := []SensorFan{
		{Name: "fan0", RPM: 0, MaxRPM: 5349},
		{Name: "fan1", RPM: 0, MaxRPM: 5777},
	}
	want := macmonSample{
		CPUTempC: &cpu, GPUTempC: &gpu, Fans: &fans,
		Power:     &SensorPower{SystemW: &systemW, CPUW: &cpuW, GPUW: &gpuW, ANEW: &aneW},
		SampledAt: time.Date(2026, time.November, 2, 6, 29, 4, 730324000, time.UTC),
	}
	// Compare instants separately because the input uses an explicit UTC offset.
	if s.SampledAt.IsZero() || !s.SampledAt.Equal(want.SampledAt) {
		t.Fatalf("SampledAt = %v, want %v", s.SampledAt, want.SampledAt)
	}
	s.SampledAt = want.SampledAt
	if !reflect.DeepEqual(s, want) {
		t.Errorf("sample = %+v, want %+v", s, want)
	}
}

func TestParseMacmonLineFanless(t *testing.T) {
	s, ok := parseMacmonLine([]byte(`{"timestamp":"2026-11-02T06:29:04Z","fans":[]}`))
	if !ok || s.Fans == nil || *s.Fans == nil || len(*s.Fans) != 0 {
		t.Fatalf("empty fan list not preserved: sample=%+v, ok=%v", s, ok)
	}
	if s.CPUTempC != nil || s.GPUTempC != nil || s.Power != nil {
		t.Fatalf("unmeasured fields populated: %+v", s)
	}
}

func TestParseMacmonLineFansAbsent(t *testing.T) {
	s, ok := parseMacmonLine([]byte(`{"timestamp":"2026-11-02T06:29:04Z","temp":{"cpu_temp_avg":40}}`))
	if !ok || s.Fans != nil {
		t.Fatalf("absent fans: sample=%+v, ok=%v", s, ok)
	}
	if s.Power != nil {
		t.Fatalf("absent power fields produced power: %+v", s.Power)
	}
}

func TestParseMacmonLineOptionalFields(t *testing.T) {
	zero, cpu, gpu, watts := 0.0, 40.5, 41.25, 3.5
	for _, tc := range []struct {
		name   string
		fields string
		want   macmonSample
	}{
		{"cpu temperature only", `"temp":{"cpu_temp_avg":40.5}`, macmonSample{CPUTempC: &cpu}},
		{"gpu temperature only", `"temp":{"gpu_temp_avg":41.25}`, macmonSample{GPUTempC: &gpu}},
		{"system power only", `"sys_power":3.5`, macmonSample{Power: &SensorPower{SystemW: &watts}}},
		{"cpu power only", `"cpu_power":3.5`, macmonSample{Power: &SensorPower{CPUW: &watts}}},
		{"gpu power only", `"gpu_power":3.5`, macmonSample{Power: &SensorPower{GPUW: &watts}}},
		{"ane power only", `"ane_power":3.5`, macmonSample{Power: &SensorPower{ANEW: &watts}}},
		{"measured zeros", `"temp":{"cpu_temp_avg":0,"gpu_temp_avg":0},"sys_power":0,"cpu_power":0,"gpu_power":0,"ane_power":0`, macmonSample{
			CPUTempC: &zero, GPUTempC: &zero,
			Power: &SensorPower{SystemW: &zero, CPUW: &zero, GPUW: &zero, ANEW: &zero},
		}},
		{"nulls are unknown", `"temp":{"cpu_temp_avg":null,"gpu_temp_avg":41.25},"fans":null,"sys_power":null,"cpu_power":null,"gpu_power":null,"ane_power":null`, macmonSample{GPUTempC: &gpu}},
		{"ignore unrelated shapes", `"temp":{"cpu_temp_avg":40.5},"cpu_tiers":"changed","ecpu_cores":{},"pcpu_cores":false,"memory":"ignored","all_power":100`, macmonSample{CPUTempC: &cpu}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ok := parseMacmonLine([]byte(`{"timestamp":"2026-11-02T06:29:04Z",` + tc.fields + `}`))
			if !ok {
				t.Fatal("usable partial sample rejected")
			}
			tc.want.SampledAt = time.Date(2026, time.November, 2, 6, 29, 4, 0, time.UTC)
			if !reflect.DeepEqual(s, tc.want) {
				t.Errorf("sample = %+v, want %+v", s, tc.want)
			}
		})
	}
}

func TestParseMacmonLineRoundsFanRPM(t *testing.T) {
	s, ok := parseMacmonLine([]byte(`{"timestamp":"2026-11-02T06:29:04Z","fans":[{"name":"fan0","rpm":1234.4,"max_rpm":5348.5},{"name":"fan1","rpm":0.5,"max_rpm":5776.6}]}`))
	want := []SensorFan{{Name: "fan0", RPM: 1234, MaxRPM: 5349}, {Name: "fan1", RPM: 1, MaxRPM: 5777}}
	if !ok || s.Fans == nil || !reflect.DeepEqual(*s.Fans, want) {
		t.Fatalf("rounded fans: sample=%+v, ok=%v; want %v", s, ok, want)
	}
}

func TestParseMacmonLineRejectsGarbage(t *testing.T) {
	for _, line := range []string{
		"not json", `{}`, `{"timestamp":"x"}`, `null`, `[]`,
		`{"temp":{"cpu_temp_avg":40}}`,
		`{"timestamp":"x","fans":[]}`,
		`{"timestamp":"2026-11-02T06:29:04Z"}`,
		`{"timestamp":"2026-11-02T06:29:04Z","temp":{}}`,
		`{"timestamp":"2026-11-02T06:29:04Z","fans":null,"ane_power":null}`,
		`{"timestamp":"2026-11-02T06:29:04Z","cpu_power":"bad"}`,
		`{"timestamp":"2026-11-02T06:29:04Z","fans":[]} trailing`,
	} {
		t.Run(line, func(t *testing.T) {
			if s, ok := parseMacmonLine([]byte(line)); ok {
				t.Fatalf("invalid or empty sample accepted: %+v", s)
			}
		})
	}
}

func TestParseMacmonLineIgnoresUnstableCPUFields(t *testing.T) {
	line := `{"timestamp":"2026-11-02T06:29:04Z","ecpu_usage":[2276,0.07],"pcpu_usage":[3658,0.06],"cpu_tiers":[{"label":"P"},{"label":"S"}],"ecpu_cores":[],"pcpu_cores":[],"memory":{"swap_total":1},"all_power":3.5}`
	if s, ok := parseMacmonLine([]byte(line)); ok {
		t.Fatalf("unused fields produced a usable sample: %+v", s)
	}
}

func TestParseThermalPressure(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
		ok    bool
	}{
		{"com.apple.system.thermalpressurelevel 0", "nominal", true},
		{"com.apple.system.thermalpressurelevel 1", "moderate", true},
		{"com.apple.system.thermalpressurelevel 2", "heavy", true},
		{"com.apple.system.thermalpressurelevel 3", "trapping", true},
		{"com.apple.system.thermalpressurelevel 4", "sleeping", true},
		{" \tcom.apple.system.thermalpressurelevel\t2\n", "heavy", true},
		{"com.apple.system.thermalpressurelevel 9", "", false},
		{"com.apple.system.thermalpressurelevel -1", "", false},
		{"com.apple.system.thermalpressurelevel x", "", false},
		{"com.apple.system.thermalpressurelevel", "", false},
		{"com.apple.system.thermalpressurelevel 2 extra", "", false},
		{"other.key 2", "", false},
		{"", "", false},
		{"garbage", "", false},
	} {
		t.Run(tc.input, func(t *testing.T) {
			if got, ok := parseThermalPressure(tc.input); got != tc.want || ok != tc.ok {
				t.Errorf("parseThermalPressure(%q) = (%q, %v), want (%q, %v)", tc.input, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestMemoryPressureName(t *testing.T) {
	for _, tc := range []struct {
		level uint32
		want  string
		ok    bool
	}{
		{1, "normal", true}, {2, "warning", true}, {4, "critical", true},
		{0, "", false}, {3, "", false}, {5, "", false}, {4294967295, "", false},
	} {
		if got, ok := memoryPressureName(tc.level); got != tc.want || ok != tc.ok {
			t.Errorf("memoryPressureName(%d) = (%q, %v), want (%q, %v)", tc.level, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseSwapUsage(t *testing.T) {
	// xsw_usage: total=1 GiB, available=1018 MiB, used=6 MiB, then page size/encryption.
	raw := []byte{
		0, 0, 0, 0x40, 0, 0, 0, 0,
		0, 0, 0xa0, 0x3f, 0, 0, 0, 0,
		0, 0, 0x60, 0, 0, 0, 0, 0,
		0, 0x10, 0, 0, 1, 0, 0, 0,
	}
	for _, length := range []int{24, 32} {
		if total, used, ok := parseSwapUsage(raw[:length]); !ok || total != 1<<30 || used != 6<<20 {
			t.Errorf("parseSwapUsage(%d bytes) = (%d, %d, %v), want (%d, %d, true)", length, total, used, ok, 1<<30, 6<<20)
		}
	}
	for length := 0; length < 24; length++ {
		if _, _, ok := parseSwapUsage(raw[:length]); ok {
			t.Errorf("accepted %d-byte truncated struct", length)
		}
	}
	if total, used, ok := parseSwapUsage(make([]byte, 24)); !ok || total != 0 || used != 0 {
		t.Errorf("zero swap = (%d, %d, %v), want (0, 0, true)", total, used, ok)
	}
}
