// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

func TestManualGPUInventoryCarriesWireFields(t *testing.T) {
	for _, tc := range []struct{ name, gpus, want string }{
		{"measured", `[{"name":"GPU","memory_kind":"dedicated","vram_bytes":8589934592,"vram_used_bytes":0,"shared_bytes":17179869184,"shared_used_bytes":0,"utilization_percent":0},{"name":"Unknown","memory_kind":"unavailable"}]`, ""},
		{"legacy", `[{"name":"Old","vram_bytes":8589934592}]`, `[{"name":"Old","memory_kind":"unavailable","vram_bytes":8589934592}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, rw, rt := newTestManager()
			entry := ManualEntry{Name: "gpu-node", Address: "node.local"}
			m.nodes[entry.Name] = &trackedNode{entry: entry, status: ManualNodeStatus{ID: entry.Name, Address: entry.Address}}
			rt.set(http.MethodGet, "node.local:14318", "/v1/node-info", func(*http.Request) (*http.Response, error) {
				return httpJSON(http.StatusOK, `{"GPUs":`+tc.gpus+`}`)
			})
			m.probeNode(entry)
			msg := readCaptureUntil(t, rw, methodIs("node/updated"))
			var got struct {
				GPUs []map[string]json.RawMessage `json:"gpus"`
			}
			if err := json.Unmarshal(msg.Params, &got); err != nil {
				t.Fatal(err)
			}
			wantWire := tc.want
			if wantWire == "" {
				wantWire = tc.gpus
			}
			var want []map[string]json.RawMessage
			if err := json.Unmarshal([]byte(wantWire), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.GPUs, want) {
				t.Fatalf("GPU transport = %s, want %s", msg.Params, wantWire)
			}
		})
	}
}

func TestManualTelemetryMixedMeasuredGPUReadings(t *testing.T) {
	for _, tc := range []struct {
		name, gpus string
		valid      bool
		want       uint32
	}{
		{"mixed", `[{"name":"Unknown"},{"name":"Known","utilization_percent":77},{"name":"Lower","utilization_percent":12}]`, true, 77},
		{"idle", `[{"name":"Unknown"},{"name":"Idle","utilization_percent":0}]`, true, 0},
		{"unknown", `[{"name":"Unknown"}]`, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, rw, rt := newTestManager()
			entry := ManualEntry{Name: "gpu-node", Address: "node.local"}
			m.nodes[entry.Name] = &trackedNode{entry: entry, status: ManualNodeStatus{ID: entry.Name, Address: entry.Address}}
			rt.set(http.MethodGet, "node.local:14318", "/v1/node-info", func(*http.Request) (*http.Response, error) {
				data, err := json.Marshal(struct {
					GPUs  json.RawMessage `json:"GPUs"`
					Valid bool            `json:"telemetryValid"`
				}{json.RawMessage(tc.gpus), tc.valid})
				if err != nil {
					return nil, err
				}
				return httpJSON(http.StatusOK, string(data))
			})
			m.probeNodeTelemetry(context.Background(), entry.Name, entry)
			msg := readCaptureUntil(t, rw, methodIs("node/telemetry"))
			got := decodeParams[NodeTelemetryNotification](t, msg)
			if got.GPUUtilizationPct != tc.want || got.TelemetryValid != tc.valid {
				t.Fatalf("telemetry = %+v", got)
			}
		})
	}
}

func gpuReading[T ~uint32 | ~uint64](value T) *T {
	return &value
}

func TestManualGPUInventoryUsesSharedEquality(t *testing.T) {
	equal := func(a, b []GPUInfo) bool {
		return gpusEqual(a, b)
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
	b[0] = a[0]
	b[0].MemoryKind = "unified"
	if equal(a, b) {
		t.Fatal("a memory kind change must trigger an update")
	}
	if !equal(nil, nil) || equal(nil, a) || equal(a, nil) {
		t.Fatal("inventory equality must account for appearance and disappearance")
	}
}
