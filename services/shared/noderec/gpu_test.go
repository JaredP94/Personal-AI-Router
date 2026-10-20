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

func TestGPUEqual(t *testing.T) {
	decode := func(wire string) GPUInfo {
		t.Helper()
		var gpu GPUInfo
		if err := json.Unmarshal([]byte(wire), &gpu); err != nil {
			t.Fatal(err)
		}
		return gpu
	}
	base := `{"name":"GPU","memory_kind":"dedicated","vram_bytes":0,"vram_used_bytes":0,"shared_bytes":0,"shared_used_bytes":0,"utilization_percent":0}`
	gpu := decode(base)
	if !gpu.Equal(gpu) || !gpu.Equal(decode(base)) {
		t.Fatal("equal readings must compare equal regardless of pointer identity")
	}
	unknown := `{"name":"GPU","memory_kind":"unavailable"}`
	if !decode(unknown).Equal(decode(unknown)) {
		t.Fatal("matching absent readings must compare equal")
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
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(base), &fields); err != nil {
				t.Fatal(err)
			}
			fields[field] = json.RawMessage(replacement)
			changed, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			other := decode(string(changed))
			if gpu.Equal(other) || other.Equal(gpu) {
				t.Fatal("changed field was ignored")
			}
			delete(fields, field)
			absent, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			other = decode(string(absent))
			if gpu.Equal(other) || other.Equal(gpu) {
				t.Fatal("absent field and measured zero must differ")
			}
			if !other.Equal(decode(string(absent))) {
				t.Fatal("matching absent fields must compare equal")
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
