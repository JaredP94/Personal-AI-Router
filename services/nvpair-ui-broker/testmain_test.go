// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Settings startup paths may consult saved state even in port-planning tests.
// Keep all component tests away from real device configuration.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "nvpair-broker-tests-*")
	if err != nil {
		panic(err)
	}
	for _, name := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "APPDATA", "LOCALAPPDATA"} {
		if err := os.Setenv(name, filepath.Join(dir, name)); err != nil {
			panic(err)
		}
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
