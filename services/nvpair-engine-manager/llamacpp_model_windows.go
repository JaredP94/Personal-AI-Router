// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
)

func openLlamaModel(_, _ string) (*os.File, error) {
	return nil, fmt.Errorf("llama.cpp model descriptor inheritance is unsupported on Windows")
}
