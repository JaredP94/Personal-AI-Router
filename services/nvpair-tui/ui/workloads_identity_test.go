// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import "testing"

// Every proxy numbers its jobs from 1, so jobs that share a number on different
// engines or runs are different jobs, and a removal (which names only the
// origin and id) drops all of them.
func TestWorkloadsKeepJobsThatShareANumberApart(t *testing.T) {
	v := &workloadsView{byKey: map[string]workload{}}
	v.upsert(workload{ID: "3", Engine: "ollama", OriginatedFrom: "x"})
	v.upsert(workload{ID: "3", Engine: "omlx", OriginatedFrom: "x"})
	v.upsert(workload{ID: "3", Engine: "omlx", RunID: "run-2", OriginatedFrom: "x"})
	v.upsert(workload{ID: "4", Engine: "omlx", OriginatedFrom: "x"})
	if len(v.byKey) != 4 || len(v.order) != 4 {
		t.Fatalf("rows = %d (order %d), want 4", len(v.byKey), len(v.order))
	}
	v.remove("x", "3")
	if len(v.byKey) != 1 || len(v.order) != 1 || v.byKey[v.order[0]].ID != "4" {
		t.Fatalf("after removing job 3: %v", v.byKey)
	}
}
