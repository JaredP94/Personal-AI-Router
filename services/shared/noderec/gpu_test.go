// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package noderec

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestGPUWireRoundTrip(t *testing.T) {
	for _, wire := range []string{
		`{"name":"Windows dedicated","memory_kind":"dedicated","vram_bytes":8589934592,"vram_used_bytes":2147483648,"shared_bytes":17179869184,"shared_used_bytes":3221225472,"utilization_percent":73}`,
		`{"name":"Idle","memory_kind":"dedicated","vram_bytes":0,"vram_used_bytes":0,"shared_bytes":0,"shared_used_bytes":0,"utilization_percent":0}`,
		`{"name":"Unified","memory_kind":"unified","vram_bytes":17179869184}`,
		`{"name":"Shared","memory_kind":"shared","vram_bytes":17179869184,"vram_used_bytes":0}`,
		`{"name":"Unknown","memory_kind":"unavailable"}`,
	} {
		t.Run(wire, func(t *testing.T) {
			var gpu GPUInfo
			if err := json.Unmarshal([]byte(wire), &gpu); err != nil {
				t.Fatal(err)
			}
			// Exercise the directory transport as well as the HTTP GPU shape.
			data, err := json.Marshal(DirectoryNode{GPUs: []GPUInfo{gpu}})
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				GPUs []map[string]json.RawMessage `json:"gpus"`
			}
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			var want map[string]json.RawMessage
			if err := json.Unmarshal([]byte(wire), &want); err != nil {
				t.Fatal(err)
			}
			if len(got.GPUs) != 1 || !reflect.DeepEqual(got.GPUs[0], want) {
				t.Fatalf("GPU fields lost or fabricated: %s, want %s", data, wire)
			}
		})
	}
}

func TestLegacyGPUDefaultsToUnavailable(t *testing.T) {
	var gpu GPUInfo
	if err := json.Unmarshal([]byte(`{"name":"Old peer","vram_bytes":8589934592,"utilization_percent":0}`), &gpu); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(gpu)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["memory_kind"]) != `"unavailable"` {
		t.Fatalf("legacy GPU must default to unavailable: %s", data)
	}
}
