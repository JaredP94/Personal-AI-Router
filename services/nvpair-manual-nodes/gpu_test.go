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

func TestGPUChangesCompareReadingsByValue(t *testing.T) {
	decode := func(wire string) []GPUInfo {
		t.Helper()
		var gpus []GPUInfo
		if err := json.Unmarshal([]byte(wire), &gpus); err != nil {
			t.Fatal(err)
		}
		return gpus
	}
	equal := func(a, b []GPUInfo) bool { return gpusEqual(a, b) }
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
