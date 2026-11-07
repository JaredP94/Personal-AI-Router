// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
)

func TestDownloadedInventorySkipsUnsupportedLlamaPlatform(t *testing.T) {
	reg := NewRegistry()
	reg.engines["llamacpp"] = &Manifest{Engine: "llamacpp"}
	ex := NewExecutor(reg, NewReporter(nil), nil, t.TempDir())
	result, err := ex.modelsResult(context.Background())
	if err != nil || len(result.DownloadedByEngine) != 0 {
		t.Fatalf("unsupported cache broke model inventory: %+v %v", result, err)
	}
}
