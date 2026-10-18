// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"reflect"
	"strings"
	"testing"
)

// Dedicated Usage must never be substituted for an adapter's shared usage.
func TestWindowsSharedAdapterDoesNotUseDedicatedCounter(t *testing.T) {
	inventory := []GPUInfo{{Name: "Shared GPU", MemoryKind: MemoryShared, VRAMBytes: byteCapacity(16 << 30), statsKey: "shared"}}
	stats := map[string]gpuStat{"shared": {VRAMUsed: 1 << 30, VRAMUsedValid: true}}
	response, body := buildResponseDecode(t, inventory, nil, 0, statsSnapshot{GPU: stats})
	if response.GPUs[0].VRAMUsedBytes != nil {
		t.Fatalf("missing Shared Usage must stay unknown, not Dedicated Usage: %v", body)
	}
}

func TestWindowsMemoryClassificationReachesWire(t *testing.T) {
	const key = "luid_0x00000000_0x0000abcd_phys_0"
	zero := uint64(0)
	cases := []struct {
		name      string
		dedicated uint64
		shared    uint64
		usage     map[string]int64
		want      GPUInfo
	}{
		{
			name:      "dedicated 8 GiB plus separate shared 16 GiB",
			dedicated: 8 << 30, shared: 16 << 30,
			usage: map[string]int64{key: 3 << 30},
			want:  GPUInfo{Name: "GPU", MemoryKind: MemoryDedicated, VRAMBytes: byteCapacity(8 << 30), VRAMUsedBytes: byteCapacity(2 << 30), SharedBytes: byteCapacity(16 << 30), SharedUsedBytes: byteCapacity(3 << 30)},
		},
		{
			name:   "zero dedicated uses shared 16 GiB as capacity",
			shared: 16 << 30, usage: map[string]int64{key: 3 << 30},
			want: GPUInfo{Name: "GPU", MemoryKind: MemoryShared, VRAMBytes: byteCapacity(16 << 30), VRAMUsedBytes: byteCapacity(3 << 30)},
		},
		{
			name:      "missing Shared Usage on dedicated adapter",
			dedicated: 8 << 30, shared: 16 << 30,
			want: GPUInfo{Name: "GPU", MemoryKind: MemoryDedicated, VRAMBytes: byteCapacity(8 << 30), VRAMUsedBytes: byteCapacity(2 << 30), SharedBytes: byteCapacity(16 << 30)},
		},
		{
			name:   "missing Shared Usage on shared adapter",
			shared: 16 << 30,
			want:   GPUInfo{Name: "GPU", MemoryKind: MemoryShared, VRAMBytes: byteCapacity(16 << 30)},
		},
		{
			name:      "measured idle shared usage on dedicated adapter",
			dedicated: 8 << 30, shared: 16 << 30, usage: map[string]int64{key: 0},
			want: GPUInfo{Name: "GPU", MemoryKind: MemoryDedicated, VRAMBytes: byteCapacity(8 << 30), VRAMUsedBytes: byteCapacity(2 << 30), SharedBytes: byteCapacity(16 << 30), SharedUsedBytes: &zero},
		},
		{
			name:   "measured idle shared usage on shared adapter",
			shared: 16 << 30, usage: map[string]int64{key: 0},
			want: GPUInfo{Name: "GPU", MemoryKind: MemoryShared, VRAMBytes: byteCapacity(16 << 30), VRAMUsedBytes: &zero},
		},
		{
			name: "DXGI zero shared budget is known",
			want: GPUInfo{Name: "GPU", MemoryKind: MemoryShared, VRAMBytes: &zero},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inventory := []GPUInfo{staticGPUFromDXGI("GPU", key, tc.dedicated, tc.shared)}
			stats, valid := decodeWindowsGPUCounters(map[string]int64{key: 2 << 30}, tc.usage, nil)
			if valid {
				t.Fatal("memory-only counters must not establish utilization validity")
			}
			response, body := buildResponseDecode(t, inventory, nil, 0, statsSnapshot{GPU: stats})
			if !reflect.DeepEqual(response.GPUs, []GPUInfo{tc.want}) {
				t.Fatalf("unexpected memory classification/usage: %+v; wire: %v", response.GPUs[0], body)
			}
		})
	}
}

func TestWindowsCountersJoinPerLUID(t *testing.T) {
	const first = "luid_0x00000000_0x0000abcd_phys_0"
	const second = "luid_0xffffffff_0xdeadbeef_phys_0"
	got, valid := decodeWindowsGPUCounters(
		map[string]int64{strings.ToUpper(first): 0, second: 2 << 30, "_Total": 999, "luid_negative": -1},
		map[string]int64{first: 3 << 30, strings.ToUpper(second): 0, "_Total": 999, "luid_negative": -2},
		map[string]float64{"pid_1_" + strings.ToUpper(first) + "_eng_0_engtype_3D": 0},
	)
	want := map[string]gpuStat{
		first:  {VRAMUsedValid: true, SharedUsed: 3 << 30, SharedUsedValid: true, UtilizationValid: true},
		second: {VRAMUsed: 2 << 30, VRAMUsedValid: true, SharedUsedValid: true},
	}
	if !valid || !reflect.DeepEqual(got, want) {
		t.Fatalf("counter joins must preserve per-adapter values and measured zero: got=%+v valid=%v", got, valid)
	}
	inventory := []GPUInfo{
		staticGPUFromDXGI("Dedicated", first, 8<<30, 16<<30),
		staticGPUFromDXGI("Shared", second, 0, 16<<30),
	}
	response, body := buildResponseDecode(t, inventory, nil, 0, statsSnapshot{GPU: got})
	if *response.GPUs[0].VRAMBytes != 8<<30 || *response.GPUs[0].VRAMUsedBytes != 0 || *response.GPUs[0].SharedUsedBytes != 3<<30 ||
		*response.GPUs[1].VRAMBytes != 16<<30 || *response.GPUs[1].VRAMUsedBytes != 0 || response.GPUs[1].SharedBytes != nil || response.GPUs[1].UtilizationPercent != nil {
		t.Fatalf("adapters or memory pools combined: %v", body)
	}
}

func TestWindowsSharedCounterWithoutDedicatedCounter(t *testing.T) {
	const key = "luid_0x00000000_0x0000abcd_phys_0"
	stats, valid := decodeWindowsGPUCounters(nil, map[string]int64{key: 4 << 30}, nil)
	inventory := []GPUInfo{staticGPUFromDXGI("GPU", key, 8<<30, 16<<30)}
	response, body := buildResponseDecode(t, inventory, nil, 0, statsSnapshot{GPU: stats})
	gpu := response.GPUs[0]
	if valid || gpu.VRAMUsedBytes != nil || gpu.SharedUsedBytes == nil || *gpu.SharedUsedBytes != 4<<30 {
		t.Fatalf("shared counter must remain independently available: %v", body)
	}
}

func TestMeasuredZeroGPUReadings(t *testing.T) {
	static, _ := parseNvidiaStatic("idle, Idle, 8192\nunknown, Unknown, 8192")
	stats, count := parseNvidiaDynamic("idle, 0, 0\nunknown, [N/A], [N/A]")
	if count != 1 {
		t.Fatalf("valid utilization count = %d, want 1", count)
	}
	snap := statsSnapshot{GPU: stats}
	body := string(buildResponse(static, nil, 0, snap, "", nil))
	if strings.Count(body, `"utilization_percent":0`) != 1 || strings.Count(body, `"vram_used_bytes":0`) != 1 {
		t.Fatalf("measured zeros must be present only on the measured adapter: %s", body)
	}
	typed, _ := buildResponseDecode(t, static, nil, 0, snap)
	if typed.GPUs[0].UtilizationPercent == nil || typed.GPUs[0].VRAMUsedBytes == nil ||
		typed.GPUs[1].UtilizationPercent != nil || typed.GPUs[1].VRAMUsedBytes != nil {
		t.Fatalf("per-adapter measurement validity lost: %s", body)
	}
}

func TestNamesOnlyGPUUnavailable(t *testing.T) {
	body := string(buildResponse([]GPUInfo{namesOnlyGPU("AMD names only")}, nil, 0, statsSnapshot{}, "", nil))
	if !strings.Contains(body, `"memory_kind":"unavailable"`) || strings.Contains(body, `"utilization_percent"`) || strings.Contains(body, `"vram_bytes"`) {
		t.Fatalf("names-only adapter must be unavailable, not idle: %s", body)
	}
}

func TestIORegistryDoesNotTreatSystemAllocationAsDedicated(t *testing.T) {
	const fixture = `<plist version="1.0"><array><dict>
<key>IORegistryEntryID</key><integer>23</integer>
<key>model</key><string>AMD Radeon Pro</string>
<key>VRAM,totalMB</key><integer>8192</integer>
<key>PerformanceStatistics</key><dict>
<key>Alloc system memory</key><integer>1073741824</integer>
</dict></dict></array></plist>`
	records, err := parseIORegistryGPUs([]byte(fixture), 32<<30)
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	if records[0].vramUsed != 0 || records[0].vramUsedValid || records[0].vramTotal != 8<<30 || records[0].memoryKind != MemoryDedicated {
		t.Fatalf("system allocation must not be reported as dedicated usage: %+v", records[0])
	}
}

func TestNvidiaMalformedCapacityOmitsMemory(t *testing.T) {
	inventory, _ := parseNvidiaStatic("gpu, NVIDIA, malformed")
	stats, _ := parseNvidiaDynamic("gpu, 20, 1024")
	response, _ := buildResponseDecode(t, inventory, nil, 0, statsSnapshot{GPU: stats})
	gpu := response.GPUs[0]
	if gpu.MemoryKind != MemoryUnavailable || gpu.VRAMBytes != nil || gpu.VRAMUsedBytes != nil {
		t.Fatalf("malformed capacity must not claim known memory: %+v", gpu)
	}
	if gpu.UtilizationPercent == nil || *gpu.UtilizationPercent != 20 {
		t.Fatalf("memory failure must not erase measured utilization: %+v", gpu)
	}
}

func TestUnifiedMemoryUsageRequiresMeasuredSystemMemory(t *testing.T) {
	inventory, _ := parseNvidiaStatic("gpu, NVIDIA GB10, [N/A]")
	for _, valid := range []bool{false, true} {
		response, _ := buildResponseDecode(t, inventory, nil, 0, statsSnapshot{MemUsedValid: valid})
		used := response.GPUs[0].VRAMUsedBytes
		if (used != nil) != valid || (used != nil && *used != 0) {
			t.Fatalf("valid=%v: unexpected system usage: %+v", valid, response.GPUs[0])
		}
	}
}

func TestIORegistryClassificationReachesWire(t *testing.T) {
	const fixture = `<plist version="1.0"><array>
<dict><key>IORegistryEntryID</key><integer>1</integer>
<key>IOObjectClass</key><string>AGXFamily</string>
<key>PerformanceStatistics</key><dict>
<key>Alloc system memory</key><integer>0</integer>
<key>Device Utilization %</key><integer>0</integer>
</dict></dict>
<dict><key>IORegistryEntryID</key><integer>2</integer>
<key>model</key><string>Apple M4</string></dict>
<dict><key>IORegistryEntryID</key><integer>3</integer>
<key>model</key><string>AMD Radeon Pro</string>
<key>VRAM,totalMB</key><integer>8192</integer>
<key>PerformanceStatistics</key><dict>
<key>Alloc system memory</key><integer>1073741824</integer>
</dict></dict></array></plist>`
	static, err := staticGPUsFromIORegistry([]byte(fixture), 32<<30)
	if err != nil {
		t.Fatal(err)
	}
	reading, err := dynamicGPUReadingFromIORegistry([]byte(fixture), 32<<30)
	if err != nil {
		t.Fatal(err)
	}
	response, _ := buildResponseDecode(t, static, nil, 0, statsSnapshot{GPU: reading.stats})
	for i, kind := range []MemoryKind{MemoryUnified, MemoryUnified, MemoryDedicated} {
		gpu := response.GPUs[i]
		wantTotal := uint64(32 << 30)
		if i == 2 {
			wantTotal = 8 << 30
		}
		if gpu.MemoryKind != kind || gpu.VRAMBytes == nil || *gpu.VRAMBytes != wantTotal {
			t.Fatalf("adapter %d classification/capacity: %+v", i, gpu)
		}
		if (gpu.VRAMUsedBytes != nil) != (i == 0) || (gpu.UtilizationPercent != nil) != (i == 0) {
			t.Fatalf("adapter %d fabricated or lost measurement: %+v", i, gpu)
		}
		if gpu.SharedBytes != nil || gpu.SharedUsedBytes != nil {
			t.Fatalf("non-Windows shared fields: %+v", gpu)
		}
	}
}

// TestParseNvidiaStatic pins the static enumeration parse: name + total VRAM
// (MiB->bytes) keyed by UUID, with skips for malformed rows, plus UMA [N/A]
// detection.
func TestParseNvidiaStatic(t *testing.T) {
	zero := uint64(0)
	cases := []struct {
		name    string
		in      string
		want    []GPUInfo
		wantUMA bool
	}{
		{
			name: "discrete VRAM",
			in: "GPU-aaa, NVIDIA GeForce RTX 4090, 24564\n" +
				"GPU-bbb, NVIDIA A100, 81920\n" +
				", MissingUUID, 4096\n" + // skipped: empty uuid
				"GPU-ccc, , 4096\n" + // skipped: empty name
				"GPU-ddd, Bad VRAM, notanumber\n", // kept, memory unavailable
			want: []GPUInfo{
				{Name: "NVIDIA GeForce RTX 4090", MemoryKind: MemoryDedicated, VRAMBytes: byteCapacity(24564 * 1024 * 1024), statsKey: "GPU-aaa"},
				{Name: "NVIDIA A100", MemoryKind: MemoryDedicated, VRAMBytes: byteCapacity(81920 * 1024 * 1024), statsKey: "GPU-bbb"},
				{Name: "Bad VRAM", MemoryKind: MemoryUnavailable, VRAMBytes: nil, statsKey: "GPU-ddd"},
			},
		},
		{
			name: "numeric zero capacity remains measured dedicated",
			in:   "GPU-zero, NVIDIA zero, 0\n",
			want: []GPUInfo{{Name: "NVIDIA zero", MemoryKind: MemoryDedicated, VRAMBytes: &zero, statsKey: "GPU-zero"}},
		},
		{
			name: "unified memory [N/A]",
			in:   "GPU-spark, NVIDIA GB10, [N/A]\n",
			want: []GPUInfo{
				{Name: "NVIDIA GB10", MemoryKind: MemoryUnified, VRAMBytes: nil, statsKey: "GPU-spark", usesSystemMemoryUsage: true},
			},
			wantUMA: true,
		},
		{
			name: "unified memory Not Supported",
			in:   "GPU-spark, NVIDIA GB10, [Not Supported]\n",
			want: []GPUInfo{
				{Name: "NVIDIA GB10", MemoryKind: MemoryUnified, VRAMBytes: nil, statsKey: "GPU-spark", usesSystemMemoryUsage: true},
			},
			wantUMA: true,
		},
		{
			name: "mixed discrete and unified memory",
			in: "GPU-aaa, NVIDIA GeForce RTX 4090, 24564\n" +
				"GPU-spark, NVIDIA GB10, [N/A]\n",
			want: []GPUInfo{
				{Name: "NVIDIA GeForce RTX 4090", MemoryKind: MemoryDedicated, VRAMBytes: byteCapacity(24564 * 1024 * 1024), statsKey: "GPU-aaa"},
				{Name: "NVIDIA GB10", MemoryKind: MemoryUnified, VRAMBytes: nil, statsKey: "GPU-spark", usesSystemMemoryUsage: true},
			},
			wantUMA: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, uma := parseNvidiaStatic(c.in)
			if uma != c.wantUMA {
				t.Fatalf("unifiedMemory = %v, want %v", uma, c.wantUMA)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("parseNvidiaStatic() = %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestParseNvidiaDynamic pins the dynamic-stats parse: per-UUID gpuStat with
// utilization clamped to 100 and memory.used converted MiB->bytes, plus a
// separate count of successfully parsed utilization samples. A numeric 0 is a
// valid idle sample; [N/A] and malformed values leave display data intact but
// must not make node-wide telemetry valid.
func TestParseNvidiaDynamic(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		want        map[string]gpuStat
		wantSamples int
	}{
		{
			name: "discrete VRAM",
			in: "GPU-aaa, 37, 8192\n" +
				"GPU-bbb, 150, 1024\n" + // util clamped to 100
				", 50, 2048\n" + // skipped: empty uuid
				"GPU-ccc, bad, alsobad\n", // kept, both zero
			want: map[string]gpuStat{
				"GPU-aaa": {UtilizationValid: true, UtilizationPct: 37, VRAMUsedValid: true, VRAMUsed: 8192 * 1024 * 1024},
				"GPU-bbb": {UtilizationValid: true, UtilizationPct: 100, VRAMUsedValid: true, VRAMUsed: 1024 * 1024 * 1024},
				"GPU-ccc": {UtilizationPct: 0, VRAMUsed: 0},
			},
			wantSamples: 2,
		},
		{
			name: "unified memory [N/A] leaves memory for response assembly",
			in:   "GPU-spark, 42, [N/A]\n",
			want: map[string]gpuStat{
				"GPU-spark": {UtilizationValid: true, UtilizationPct: 42},
			},
			wantSamples: 1,
		},
		{
			name: "all unavailable utilization remains invalid",
			in: "GPU-spark, [N/A], [N/A]\n" +
				"GPU-memory, N/A, 2048\n",
			want: map[string]gpuStat{
				"GPU-spark":  {},
				"GPU-memory": {VRAMUsedValid: true, VRAMUsed: 2048 * 1024 * 1024},
			},
			wantSamples: 0,
		},
		{
			name: "valid idle sample survives mixed unavailable rows",
			in: "GPU-idle, 0, [N/A]\n" +
				"GPU-unknown, [N/A], 1024\n",
			want: map[string]gpuStat{
				"GPU-idle":    {UtilizationValid: true, UtilizationPct: 0},
				"GPU-unknown": {VRAMUsedValid: true, VRAMUsed: 1024 * 1024 * 1024},
			},
			wantSamples: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, samples := parseNvidiaDynamic(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("parseNvidiaDynamic() = %+v, want %+v", got, c.want)
			}
			if samples != c.wantSamples {
				t.Fatalf("parseNvidiaDynamic() samples = %d, want %d", samples, c.wantSamples)
			}
		})
	}
}

// TestIsNvidiaSmiNA pins recognition of nvidia-smi not-applicable sentinels.
func TestIsNvidiaSmiNA(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"[N/A]", true},
		{"N/A", true},
		{"[Not Supported]", true},
		{"Not Supported", true},
		{"8192", false},
		{"notanumber", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isNvidiaSmiNA(c.in); got != c.want {
			t.Fatalf("isNvidiaSmiNA(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
