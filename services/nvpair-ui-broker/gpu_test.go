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

func TestDiscoveryGPUInventoryUsesSharedEquality(t *testing.T) {
	equal := func(a, b []GPUInfo) bool {
		return enrichedNodeEqual(EnrichedNode{GPUs: a}, EnrichedNode{GPUs: b})
	}
	// Separate pointers with the same reading must not trigger an update.
	idleA, idleB := uint32(0), uint32(0)
	a := []GPUInfo{{MemoryKind: "dedicated", UtilizationPercent: &idleA}}
	b := []GPUInfo{{MemoryKind: "dedicated", UtilizationPercent: &idleB}}
	if !equal(a, b) {
		t.Fatal("equal readings at different addresses must compare equal")
	}
	b[0].UtilizationPercent = nil
	if equal(a, b) {
		t.Fatal("an unknown reading must differ from measured idle")
	}
	if !equal(nil, nil) || equal(nil, a) || equal(a, nil) {
		t.Fatal("inventory equality must account for appearance and disappearance")
	}
}
