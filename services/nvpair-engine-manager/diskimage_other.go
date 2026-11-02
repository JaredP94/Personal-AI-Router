// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !darwin

package main

import (
	"context"
	"errors"
)

func installAppFromDiskImage(context.Context, string, *DiskImage) (bool, error) {
	return false, errors.New("disk image installs are macOS-only")
}
