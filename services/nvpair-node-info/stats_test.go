// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// TestParseEngineInstance pins the PDH "GPU Engine" instance-name format.
// If this ever drifts, aggregateUtilization would silently produce an
// empty map for every adapter and every node would report
// utilization_percent=0. The table covers: the typical shape, the vendor-
// specific "Compute_0" suffix (NVIDIA's way of distinguishing async
// compute queues, which PDH just lets through as part of engtype),
// a few malformed variants we must reject, and the edge case of a
// trailing underscore with no engine type.
func TestParseEngineInstance(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantLuid string
		wantType string
		wantOK   bool
	}{
		{
			name:     "typical 3D engine",
			in:       "pid_1234_luid_0x00000000_0x000054f0_phys_0_eng_0_engtype_3D",
			wantLuid: "luid_0x00000000_0x000054f0_phys_0",
			wantType: "3d",
			wantOK:   true,
		},
		{
			name:     "compute variant",
			in:       "pid_99_luid_0x00000000_0x0000abcd_phys_0_eng_2_engtype_Compute_0",
			wantLuid: "luid_0x00000000_0x0000abcd_phys_0",
			wantType: "compute_0",
			wantOK:   true,
		},
		{
			name:     "mixed-case hex normalizes to lower",
			in:       "pid_1_luid_0x00000000_0x000054F0_phys_0_eng_0_engtype_3D",
			wantLuid: "luid_0x00000000_0x000054f0_phys_0",
			wantType: "3d",
			wantOK:   true,
		},
		{"missing luid prefix", "pid_1_eng_0_engtype_3D", "", "", false},
		{"missing eng segment", "pid_1_luid_0x0_0x0_phys_0_engtype_3D", "", "", false},
		{"missing engtype marker", "pid_1_luid_0x0_0x0_phys_0_eng_0", "", "", false},
		{"empty engtype value", "pid_1_luid_0x0_0x0_phys_0_eng_0_engtype_", "", "", false},
		{"unrelated garbage", "completely unrelated string", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			luid, et, ok := parseEngineInstance(c.in)
			if ok != c.wantOK || luid != c.wantLuid || et != c.wantType {
				t.Fatalf("parseEngineInstance(%q) = %q,%q,%v; want %q,%q,%v",
					c.in, luid, et, ok, c.wantLuid, c.wantType, c.wantOK)
			}
		})
	}
}

// TestAggregateUtilization exercises Task Manager's three-step algorithm:
// sum by (luid, engtype), clamp to 100, max across engtypes per luid.
// Each test case targets one specific step so a regression in any of
// them produces a pinpointable failure.
func TestAggregateUtilization(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]float64
		want map[string]uint32
	}{
		{
			name: "empty input",
			in:   map[string]float64{},
			want: map[string]uint32{},
		},
		{
			name: "single engine single process rounds half-up",
			in: map[string]float64{
				"pid_1_luid_0x0_0x1_phys_0_eng_0_engtype_3D": 42.5,
			},
			want: map[string]uint32{
				"luid_0x0_0x1_phys_0": 43,
			},
		},
		{
			name: "sum across processes on same engine",
			in: map[string]float64{
				"pid_1_luid_0x0_0x1_phys_0_eng_0_engtype_3D": 30,
				"pid_2_luid_0x0_0x1_phys_0_eng_0_engtype_3D": 20,
			},
			want: map[string]uint32{
				"luid_0x0_0x1_phys_0": 50,
			},
		},
		{
			name: "max across engines selects busiest",
			in: map[string]float64{
				"pid_1_luid_0x0_0x1_phys_0_eng_0_engtype_3D":      10,
				"pid_1_luid_0x0_0x1_phys_0_eng_1_engtype_Compute": 80,
				"pid_1_luid_0x0_0x1_phys_0_eng_2_engtype_Copy":    5,
			},
			want: map[string]uint32{
				"luid_0x0_0x1_phys_0": 80,
			},
		},
		{
			name: "sum exceeding 100 is clamped before max",
			in: map[string]float64{
				"pid_1_luid_0x0_0x1_phys_0_eng_0_engtype_3D": 75,
				"pid_2_luid_0x0_0x1_phys_0_eng_0_engtype_3D": 60,
			},
			want: map[string]uint32{
				"luid_0x0_0x1_phys_0": 100,
			},
		},
		{
			name: "multiple GPUs report independently",
			in: map[string]float64{
				"pid_1_luid_0x0_0x1_phys_0_eng_0_engtype_3D": 20,
				"pid_1_luid_0x0_0x2_phys_0_eng_0_engtype_3D": 90,
			},
			want: map[string]uint32{
				"luid_0x0_0x1_phys_0": 20,
				"luid_0x0_0x2_phys_0": 90,
			},
		},
		{
			name: "negatives and unparseable instances are dropped",
			in: map[string]float64{
				"pid_1_luid_0x0_0x1_phys_0_eng_0_engtype_3D": -5,
				"garbage": 99,
				"pid_2_luid_0x0_0x1_phys_0_eng_0_engtype_3D": 10,
			},
			want: map[string]uint32{
				"luid_0x0_0x1_phys_0": 10,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := aggregateUtilization(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("aggregateUtilization(%v) = %v; want %v", c.in, got, c.want)
			}
		})
	}
}

func TestApplyGPUStatsRetainsLastUsableSample(t *testing.T) {
	sampledAt := time.Unix(1_700_000_000, 0)
	previous := statsSnapshot{
		GPU: map[string]gpuStat{
			"gpu-a": {VRAMUsedValid: true, VRAMUsed: 4 << 30, UtilizationValid: true, UtilizationPct: 73},
		},
		GPUSampledAt: sampledAt,
	}
	next := &statsSnapshot{}

	applyGPUStats(previous, next, map[string]gpuStat{
		"gpu-a": {VRAMUsedValid: true, VRAMUsed: 5 << 30},
	}, time.Time{})

	if !reflect.DeepEqual(next.GPU, previous.GPU) {
		t.Fatalf("failed collection replaced last usable GPU sample: got %v want %v", next.GPU, previous.GPU)
	}
	if !next.GPUSampledAt.Equal(sampledAt) {
		t.Fatalf("failed collection moved sample time to %v, want %v", next.GPUSampledAt, sampledAt)
	}
}

func TestApplyGPUStatsPublishesIdleAndPreSampleFields(t *testing.T) {
	partial := map[string]gpuStat{"gpu-a": {VRAMUsedValid: true, VRAMUsed: 2 << 30}}
	beforeFirstSample := &statsSnapshot{}
	applyGPUStats(statsSnapshot{}, beforeFirstSample, partial, time.Time{})
	if !reflect.DeepEqual(beforeFirstSample.GPU, partial) || !beforeFirstSample.GPUSampledAt.IsZero() {
		t.Fatalf("pre-sample fields = %+v, want partial GPU data with no sample time", beforeFirstSample)
	}

	sampledAt := time.Unix(1_700_000_000, 123)
	idle := map[string]gpuStat{"gpu-a": {UtilizationValid: true, UtilizationPct: 0}}
	usable := &statsSnapshot{}
	applyGPUStats(statsSnapshot{}, usable, idle, sampledAt)
	if !reflect.DeepEqual(usable.GPU, idle) || !usable.GPUSampledAt.Equal(sampledAt) {
		t.Fatalf("idle sample = %+v, want valid zero-utilization sample at %v", usable, sampledAt)
	}
}

func TestApplyGPUStatsIncrementsSampleSeqOnlyOnUsableSample(t *testing.T) {
	initial := statsSnapshot{}
	first := &statsSnapshot{}
	sampledAt1 := time.Unix(1_700_000_000, 0)
	applyGPUStats(initial, first, map[string]gpuStat{"gpu-a": {UtilizationValid: true, UtilizationPct: 10}}, sampledAt1)
	if first.GPUSampleSeq != 1 {
		t.Fatalf("first sample seq = %d, want 1", first.GPUSampleSeq)
	}

	// Failed collection preserves sampleSeq
	failed := &statsSnapshot{}
	applyGPUStats(*first, failed, nil, time.Time{})
	if failed.GPUSampleSeq != 1 {
		t.Fatalf("failed collection sample seq = %d, want 1", failed.GPUSampleSeq)
	}

	// Second usable sample increments
	second := &statsSnapshot{}
	sampledAt2 := time.Unix(1_700_000_002, 0)
	applyGPUStats(*failed, second, map[string]gpuStat{"gpu-a": {UtilizationValid: true, UtilizationPct: 20}}, sampledAt2)
	if second.GPUSampleSeq != 2 {
		t.Fatalf("second sample seq = %d, want 2", second.GPUSampleSeq)
	}
}

func TestBuildResponseSampleSeqOmittedWhenZero(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	bodyWithSeq := buildResponseAt(
		[]GPUInfo{{Name: "GPU 0", MemoryKind: MemoryDedicated, statsKey: "gpu-a"}},
		nil,
		0,
		statsSnapshot{
			GPU:          map[string]gpuStat{"gpu-a": {UtilizationValid: true, UtilizationPct: 10}},
			GPUSampledAt: now,
			GPUSampleSeq: 42,
		},
		"",
		nil,
		now,
	)
	var rawWithSeq map[string]any
	if err := json.Unmarshal(bodyWithSeq, &rawWithSeq); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	seqVal, ok := rawWithSeq["sampleSeq"].(float64)
	if !ok || uint64(seqVal) != 42 {
		t.Fatalf("sampleSeq = %v, want 42", rawWithSeq["sampleSeq"])
	}

	bodyZeroSeq := buildResponseAt(
		[]GPUInfo{{Name: "GPU 0", MemoryKind: MemoryDedicated, statsKey: "gpu-a"}},
		nil,
		0,
		statsSnapshot{
			GPU:          map[string]gpuStat{"gpu-a": {UtilizationValid: true, UtilizationPct: 10}},
			GPUSampledAt: now,
			GPUSampleSeq: 0,
		},
		"",
		nil,
		now,
	)
	var rawZeroSeq map[string]any
	if err := json.Unmarshal(bodyZeroSeq, &rawZeroSeq); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := rawZeroSeq["sampleSeq"]; present {
		t.Fatalf("sampleSeq was not omitted when 0: %v", rawZeroSeq["sampleSeq"])
	}
}

func TestBuildResponseTelemetryFreshness(t *testing.T) {
	now := time.Unix(1_700_000_000, 500_000_000)
	cases := []struct {
		name      string
		sampledAt time.Time
		wantValid bool
		wantAge   int64
	}{
		{name: "no sample", wantValid: false, wantAge: 0},
		{name: "valid idle sample", sampledAt: now.Add(-137 * time.Millisecond), wantValid: true, wantAge: 137},
		{name: "future sample clamps age", sampledAt: now.Add(time.Millisecond), wantValid: true, wantAge: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := buildResponseAt(
				[]GPUInfo{{Name: "GPU 0", MemoryKind: MemoryDedicated, statsKey: "gpu-a"}},
				nil,
				0,
				statsSnapshot{
					GPU:          map[string]gpuStat{"gpu-a": {UtilizationValid: true, UtilizationPct: 0}},
					GPUSampledAt: c.sampledAt,
				},
				"",
				nil,
				now,
			)
			var typed NodeInfoResponse
			if err := json.Unmarshal(body, &typed); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if typed.TelemetryValid != c.wantValid || typed.MSSince != c.wantAge {
				t.Fatalf("telemetry = valid:%v age:%d, want valid:%v age:%d",
					typed.TelemetryValid, typed.MSSince, c.wantValid, c.wantAge)
			}

			var raw map[string]any
			if err := json.Unmarshal(body, &raw); err != nil {
				t.Fatalf("decode raw response: %v", err)
			}
			if _, ok := raw["telemetryValid"]; !ok {
				t.Fatal("telemetryValid missing from response")
			}
			if _, ok := raw["msSince"]; !ok {
				t.Fatal("msSince missing from response")
			}
		})
	}
}

// buildResponseDecode is a test helper that marshals through the real
// JSON path so omitempty behavior is part of the assertion — e.g. a
// GPU whose stats are zero must produce a response with the field
// absent, not `"vram_used_bytes":0`; a nil CPUInfo must produce JSON
// with no "cpu" key at all, not `"cpu":null`.
func buildResponseDecode(t *testing.T, static []GPUInfo, cpu *CPUInfo, memTotal uint64, snap statsSnapshot) (NodeInfoResponse, map[string]any) {
	t.Helper()
	body := buildResponse(static, cpu, memTotal, snap, "", nil)
	var typed NodeInfoResponse
	if err := json.Unmarshal(body, &typed); err != nil {
		t.Fatalf("typed decode: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("raw decode: %v", err)
	}
	return typed, raw
}

// TestBuildResponseMerge verifies the happy path: matched LUIDs get
// dynamic fields from the stats map, unmatched LUIDs stay unknown.
func TestBuildResponseMerge(t *testing.T) {
	static := []GPUInfo{
		{Name: "GPU 0", MemoryKind: MemoryDedicated, VRAMBytes: byteCapacity(16 << 30), statsKey: "luid_a"},
		{Name: "GPU 1", MemoryKind: MemoryDedicated, VRAMBytes: byteCapacity(8 << 30), statsKey: "luid_b"},
	}
	snap := statsSnapshot{
		GPU: map[string]gpuStat{
			"luid_a": {VRAMUsedValid: true, VRAMUsed: 4 << 30, UtilizationValid: true, UtilizationPct: 27},
			// luid_b intentionally absent — must stay unknown.
		},
	}
	typed, _ := buildResponseDecode(t, static, nil, 0, snap)

	if len(typed.GPUs) != 2 {
		t.Fatalf("got %d GPUs, want 2", len(typed.GPUs))
	}
	if got := typed.GPUs[0].VRAMUsedBytes; got == nil || *got != 4<<30 {
		t.Errorf("gpu 0 VRAMUsedBytes = %v, want %d", got, 4<<30)
	}
	if got := typed.GPUs[0].UtilizationPercent; got == nil || *got != 27 {
		t.Errorf("gpu 0 UtilizationPercent = %v, want 27", got)
	}
	if got := typed.GPUs[1].VRAMUsedBytes; got != nil {
		t.Errorf("gpu 1 VRAMUsedBytes = %v, want nil (unmatched LUID)", got)
	}
	if got := typed.GPUs[1].UtilizationPercent; got != nil {
		t.Errorf("gpu 1 UtilizationPercent = %v, want nil (unmatched LUID)", got)
	}
}

// TestBuildResponseUnifiedMemoryUsesSystemSnapshot verifies that UMA memory
// usage comes from the independently collected system-memory snapshot. It must
// remain available when nvidia-smi produced no dynamic GPU row, and a matching
// row may contribute utilization but must not replace the shared-memory value
// with a dedicated-VRAM counter.
func TestBuildResponseUnifiedMemoryUsesSystemSnapshot(t *testing.T) {
	const sysMemUsed uint64 = 48 << 30
	static := []GPUInfo{{
		Name: "NVIDIA GB10", MemoryKind: MemoryUnified,
		VRAMBytes:             byteCapacity(128 << 30),
		statsKey:              "GPU-spark",
		usesSystemMemoryUsage: true,
	}}
	cases := []struct {
		name     string
		gpuStats map[string]gpuStat
		wantUtil uint32
	}{
		{
			name:     "without dynamic nvidia-smi row",
			gpuStats: nil,
		},
		{
			name: "with dynamic utilization row",
			gpuStats: map[string]gpuStat{
				"GPU-spark": {VRAMUsedValid: true, VRAMUsed: 1 << 30, UtilizationValid: true, UtilizationPct: 42},
			},
			wantUtil: 42,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			snap := statsSnapshot{GPU: c.gpuStats, MemUsedBytes: sysMemUsed, MemUsedValid: true}
			typed, raw := buildResponseDecode(t, static, nil, 0, snap)
			if got := typed.GPUs[0].VRAMUsedBytes; got == nil || *got != sysMemUsed {
				t.Errorf("VRAMUsedBytes = %v, want system memory %d", got, sysMemUsed)
			}
			if got := typed.GPUs[0].UtilizationPercent; (got == nil && c.gpuStats != nil) || (got != nil && *got != c.wantUtil) {
				t.Errorf("UtilizationPercent = %v, want %d", got, c.wantUtil)
			}

			gpus, ok := raw["GPUs"].([]any)
			if !ok || len(gpus) != 1 {
				t.Fatalf("unexpected GPUs payload: %v", raw["GPUs"])
			}
			obj, ok := gpus[0].(map[string]any)
			if !ok {
				t.Fatalf("gpu 0 not an object: %T", gpus[0])
			}
			if _, present := obj["vram_used_bytes"]; !present {
				t.Errorf("vram_used_bytes should be present for unified memory: %v", obj)
			}
		})
	}
}

// Apple Silicon shares physical memory but ioreg reports the GPU-specific
// mapped allocation. It must flow through the normal per-adapter stats path,
// not be replaced with whole-system memory usage.
func TestBuildResponseAppleUnifiedMemoryUsesGPUAllocation(t *testing.T) {
	static := []GPUInfo{{
		Name: "Apple M3 Max", MemoryKind: MemoryUnified,
		VRAMBytes: byteCapacity(36 << 30),
		statsKey:  "ioreg:2a",
	}}
	snap := statsSnapshot{
		GPU: map[string]gpuStat{
			"ioreg:2a": {VRAMUsedValid: true, VRAMUsed: 8 << 30, UtilizationValid: true, UtilizationPct: 73},
		},
		MemUsedBytes: 24 << 30,
	}
	typed, _ := buildResponseDecode(t, static, nil, 0, snap)
	gpu := typed.GPUs[0]
	if (gpu.VRAMUsedBytes == nil || *gpu.VRAMUsedBytes != 8<<30) || (gpu.UtilizationPercent == nil || *gpu.UtilizationPercent != 73) {
		t.Fatalf("unexpected Apple GPU metrics: %+v", gpu)
	}
}

func TestBuildResponseRecoversDarwinGPUInventory(t *testing.T) {
	snap := statsSnapshot{
		GPU: map[string]gpuStat{
			"ioreg:2a": {VRAMUsedValid: true, VRAMUsed: 8 << 30, UtilizationValid: true, UtilizationPct: 73},
		},
		GPUInventory: []GPUInfo{{
			Name: "Apple M3 Max", MemoryKind: MemoryUnified,
			VRAMBytes: byteCapacity(36 << 30),
			statsKey:  "ioreg:2a",
		}},
	}
	typed, _ := buildResponseDecode(t, nil, nil, 0, snap)
	if len(typed.GPUs) != 1 {
		t.Fatalf("recovered GPU count = %d, want 1", len(typed.GPUs))
	}
	gpu := typed.GPUs[0]
	if gpu.Name != "Apple M3 Max" || (gpu.VRAMBytes == nil || *gpu.VRAMBytes != 36<<30) ||
		(gpu.VRAMUsedBytes == nil || *gpu.VRAMUsedBytes != 8<<30) || (gpu.UtilizationPercent == nil || *gpu.UtilizationPercent != 73) {
		t.Fatalf("unexpected recovered GPU: %+v", gpu)
	}

	static := []GPUInfo{{Name: "Apple M3 Max", MemoryKind: MemoryUnified, statsKey: "ioreg:2a"}}
	if merged := mergeGPUInventory(static, snap.GPUInventory); len(merged) != 1 ||
		(merged[0].VRAMBytes == nil || *merged[0].VRAMBytes != 36<<30) {
		t.Fatalf("matching recovered GPU did not enrich in place: %+v", merged)
	}
}

// TestBuildResponseOmitsUnknown confirms that a GPU with no stats match
// produces JSON without the dynamic fields, not with literal zeros.
// Clients depend on this to distinguish "unknown / unavailable" from
// "actually idle" for vram_used_bytes.
func TestBuildResponseOmitsUnknown(t *testing.T) {
	static := []GPUInfo{{Name: "GPU 0", MemoryKind: MemoryDedicated, VRAMBytes: byteCapacity(4 << 30), statsKey: "luid_a"}}
	_, raw := buildResponseDecode(t, static, nil, 0, statsSnapshot{})

	gpus, ok := raw["GPUs"].([]any)
	if !ok || len(gpus) != 1 {
		t.Fatalf("unexpected GPUs payload: %v", raw["GPUs"])
	}
	obj, ok := gpus[0].(map[string]any)
	if !ok {
		t.Fatalf("gpu 0 not an object: %T", gpus[0])
	}
	if _, present := obj["vram_used_bytes"]; present {
		t.Errorf("vram_used_bytes should be absent when unknown, got: %v", obj)
	}
	if _, present := obj["utilization_percent"]; present {
		t.Errorf("utilization_percent should be absent when unknown, got: %v", obj)
	}
}

// TestBuildResponseCPUMemoryMatrix exercises the four combinations of
// cpu/memory presence and absence so the pointer-omitempty behavior is
// pinned down: a nil *CPUInfo or a zero memTotal must drop the entire
// top-level object, not produce an empty shell like `"cpu":{}` or
// `"memory":{"total_bytes":0}`. Clients distinguish "host can't
// introspect this subsystem" from "subsystem reports zero" exactly the
// way they already do for GPU dynamic fields.
func TestBuildResponseCPUMemoryMatrix(t *testing.T) {
	cpu := &CPUInfo{Name: "Intel Core i9-13900K", Cores: 24}
	const memTotal uint64 = 32 << 30
	snap := statsSnapshot{
		GPU:          map[string]gpuStat{},
		CPUUtilPct:   42,
		MemUsedBytes: 10 << 30,
	}

	cases := []struct {
		name         string
		cpu          *CPUInfo
		memTotal     uint64
		snap         statsSnapshot
		wantCPUKey   bool
		wantMemKey   bool
		wantCPUUtil  uint32
		wantCPUName  string
		wantCPUCores uint32
		wantMemTotal uint64
		wantMemUsed  uint64
	}{
		{
			name:         "cpu and memory both present",
			cpu:          cpu,
			memTotal:     memTotal,
			snap:         snap,
			wantCPUKey:   true,
			wantMemKey:   true,
			wantCPUUtil:  42,
			wantCPUName:  cpu.Name,
			wantCPUCores: 24,
			wantMemTotal: memTotal,
			wantMemUsed:  10 << 30,
		},
		{
			name:         "cpu only",
			cpu:          cpu,
			memTotal:     0,
			snap:         statsSnapshot{CPUUtilPct: 42},
			wantCPUKey:   true,
			wantMemKey:   false,
			wantCPUUtil:  42,
			wantCPUName:  cpu.Name,
			wantCPUCores: 24,
		},
		{
			name:         "memory only",
			cpu:          nil,
			memTotal:     memTotal,
			snap:         statsSnapshot{MemUsedBytes: 10 << 30},
			wantCPUKey:   false,
			wantMemKey:   true,
			wantMemTotal: memTotal,
			wantMemUsed:  10 << 30,
		},
		{
			name:       "neither",
			cpu:        nil,
			memTotal:   0,
			snap:       statsSnapshot{},
			wantCPUKey: false,
			wantMemKey: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			typed, raw := buildResponseDecode(t, nil, c.cpu, c.memTotal, c.snap)

			_, cpuPresent := raw["cpu"]
			if cpuPresent != c.wantCPUKey {
				t.Errorf("cpu key present = %v, want %v (raw=%v)", cpuPresent, c.wantCPUKey, raw)
			}
			_, memPresent := raw["memory"]
			if memPresent != c.wantMemKey {
				t.Errorf("memory key present = %v, want %v (raw=%v)", memPresent, c.wantMemKey, raw)
			}

			if c.wantCPUKey {
				if typed.CPU == nil {
					t.Fatalf("typed.CPU is nil but cpu key was present in raw JSON")
				}
				if typed.CPU.Name != c.wantCPUName {
					t.Errorf("cpu.Name = %q, want %q", typed.CPU.Name, c.wantCPUName)
				}
				if typed.CPU.Cores != c.wantCPUCores {
					t.Errorf("cpu.Cores = %d, want %d", typed.CPU.Cores, c.wantCPUCores)
				}
				if typed.CPU.UtilizationPercent != c.wantCPUUtil {
					t.Errorf("cpu.UtilizationPercent = %d, want %d",
						typed.CPU.UtilizationPercent, c.wantCPUUtil)
				}
			} else if typed.CPU != nil {
				t.Errorf("typed.CPU = %+v, want nil", typed.CPU)
			}

			if c.wantMemKey {
				if typed.Memory == nil {
					t.Fatalf("typed.Memory is nil but memory key was present in raw JSON")
				}
				if typed.Memory.TotalBytes != c.wantMemTotal {
					t.Errorf("memory.TotalBytes = %d, want %d", typed.Memory.TotalBytes, c.wantMemTotal)
				}
				if typed.Memory.UsedBytes != c.wantMemUsed {
					t.Errorf("memory.UsedBytes = %d, want %d", typed.Memory.UsedBytes, c.wantMemUsed)
				}
			} else if typed.Memory != nil {
				t.Errorf("typed.Memory = %+v, want nil", typed.Memory)
			}
		})
	}
}
