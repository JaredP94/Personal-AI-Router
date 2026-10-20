// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workerhealth

import (
	"encoding/json"
	"testing"
)

func TestOKWireResult(t *testing.T) {
	data, err := json.Marshal(OK())
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"ok":true}` {
		t.Fatalf("health result = %s, want {\"ok\":true}", data)
	}
	var result Result
	if err := json.Unmarshal(data, &result); err != nil || !result.OK {
		t.Fatalf("health result did not round-trip: %+v, error = %v", result, err)
	}
}
