//go:build !darwin

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
)

func swapArchiveDirectories(stage, target string) error {
	return fmt.Errorf("atomic archive updates are supported on macOS only")
}
func renameArchiveNew(stage, target string) error {
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		return fmt.Errorf("archive destination already exists")
	}
	return os.Rename(stage, target)
}
func verifyArchivePlatform(context.Context, string, *ArchiveInstall) error {
	return fmt.Errorf("archive signature and quarantine verification require macOS")
}
