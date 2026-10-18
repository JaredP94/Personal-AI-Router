// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package relay

import (
	"encoding/json"
	"testing"

	"nvpair-shared/noderec"
)

func TestGPUChangesCompareReadingsByValue(t *testing.T) {
	decode := func(wire string) []noderec.GPUInfo {
		t.Helper()
		var gpus []noderec.GPUInfo
		if err := json.Unmarshal([]byte(wire), &gpus); err != nil {
			t.Fatal(err)
		}
		return gpus
	}
	equal := func(a, b []noderec.GPUInfo) bool {
		return directoryNodeEqual(noderec.DirectoryNode{GPUs: a}, noderec.DirectoryNode{GPUs: b})
	}
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
