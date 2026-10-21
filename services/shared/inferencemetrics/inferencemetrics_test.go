// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package inferencemetrics

import (
	"encoding/json"
	"testing"
)

func TestMeasurementOmitsAbsentFields(t *testing.T) {
	m := Measurement{PrefillTokens: IntPtr(10)}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"prefillTokens":10}`
	if string(b) != want {
		t.Fatalf("got %s want %s", b, want)
	}
}

func TestMeasurementKeepsMeasuredZero(t *testing.T) {
	m := Measurement{DecodeTokens: IntPtr(0)}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"decodeTokens":0}`
	if string(b) != want {
		t.Fatalf("got %s want %s", b, want)
	}
}
