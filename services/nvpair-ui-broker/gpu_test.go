// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"nvpair-shared/noderec"
)

func TestBrokerManualGPUProjectionAndTelemetry(t *testing.T) {
	for _, tc := range []struct {
		name, gpus string
		valid      bool
		want       uint32
	}{
		{"mixed", `[{"name":"Unknown","memory_kind":"unavailable"},{"name":"GPU","memory_kind":"dedicated","vram_bytes":8589934592,"vram_used_bytes":0,"shared_bytes":17179869184,"shared_used_bytes":0,"utilization_percent":77}]`, true, 77},
		{"idle", `[{"name":"Unknown","memory_kind":"unavailable"},{"name":"Idle","memory_kind":"shared","vram_bytes":17179869184,"vram_used_bytes":0,"utilization_percent":0}]`, true, 0},
		{"unknown", `[{"name":"Unknown","memory_kind":"unavailable"}]`, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var status manualNodeStatus
			if err := json.Unmarshal([]byte(`{"id":"manual","hostUuid":"node-a","gpus":`+tc.gpus+`}`), &status); err != nil {
				t.Fatal(err)
			}
			status.TelemetryValid = tc.valid
			enriched := manualToEnriched(status)
			b := &Broker{}
			directory := b.manualToDirectoryNode(status, "node-a")
			if !reflect.DeepEqual(enriched.GPUs, status.GPUs) || !reflect.DeepEqual(directory.GPUs, status.GPUs) {
				t.Fatal("GPU projection lost fields")
			}
			telemetry := manualNodeTelemetry(status, "node-a")
			if telemetry.GPUUtilizationPct != tc.want || telemetry.TelemetryValid != tc.valid {
				t.Fatalf("telemetry = %+v", telemetry)
			}
			data, err := json.Marshal(directory)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				GPUs []map[string]json.RawMessage `json:"gpus"`
			}
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			var want []map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tc.gpus), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.GPUs, want) {
				t.Fatalf("GPU broker transport = %s, want %s", data, tc.gpus)
			}
		})
	}
	var old manualNodeStatus
	if err := json.Unmarshal([]byte(`{"gpus":[{"name":"Old","vram_bytes":8589934592}]}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.GPUs[0].MemoryKind != noderec.MemoryUnavailable {
		t.Fatal("legacy manual status must decode as unavailable")
	}
}

func gpuReading[T ~uint32 | ~uint64](value T) *T {
	return &value
}

func TestGPUChangesCompareReadingsByValue(t *testing.T) {
	decode := func(wire string) []GPUInfo {
		t.Helper()
		var gpus []GPUInfo
		if err := json.Unmarshal([]byte(wire), &gpus); err != nil {
			t.Fatal(err)
		}
		return gpus
	}
	equal := func(a, b []GPUInfo) bool { return enrichedNodeEqual(EnrichedNode{GPUs: a}, EnrichedNode{GPUs: b}) }
	base := `[{"name":"GPU","memory_kind":"dedicated","vram_bytes":8589934592,"vram_used_bytes":0,"shared_bytes":17179869184,"shared_used_bytes":0,"utilization_percent":0}]`
	if !equal(decode(base), decode(base)) {
		t.Fatal("equal readings from separate responses must compare equal")
	}
	for field, replacement := range map[string]string{
		"name":                `"Other"`,
		"memory_kind":         `"unified"`,
		"vram_bytes":          "1",
		"vram_used_bytes":     "1",
		"shared_bytes":        "1",
		"shared_used_bytes":   "1",
		"utilization_percent": "1",
	} {
		t.Run(field, func(t *testing.T) {
			var fields []map[string]json.RawMessage
			if err := json.Unmarshal([]byte(base), &fields); err != nil {
				t.Fatal(err)
			}
			fields[0][field] = json.RawMessage(replacement)
			changed, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			if equal(decode(base), decode(string(changed))) {
				t.Fatal("changed field was ignored")
			}
			delete(fields[0], field)
			absent, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			if equal(decode(base), decode(string(absent))) {
				t.Fatal("absent field and measured value must differ")
			}
		})
	}
	if !equal(nil, nil) {
		t.Fatal("nil inventories differ")
	}
	if equal(nil, decode(base)) {
		t.Fatal("inventory appearance must differ")
	}
}
