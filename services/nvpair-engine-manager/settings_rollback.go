// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
)

// Preserve the exact override, including absent fields and unrelated platform
// settings. Reconstructing it from resolved runtime values loses that distinction.
type settingsOverride struct {
	path   string
	data   []byte
	mode   os.FileMode
	exists bool
}

func captureSettingsOverride(dir, engine string) (settingsOverride, error) {
	previous := settingsOverride{}
	if dir == "" || engine == "omlx" {
		return previous, nil
	}
	previous.path = filepath.Join(dir, engine+".json")
	data, err := os.ReadFile(previous.path)
	if os.IsNotExist(err) {
		return previous, nil
	}
	if err != nil {
		return previous, err
	}
	info, err := os.Stat(previous.path)
	if err != nil {
		return previous, err
	}
	previous.data, previous.mode, previous.exists = data, info.Mode().Perm(), true
	return previous, nil
}

func (previous settingsOverride) restore() error {
	if previous.path == "" {
		return nil
	}
	current, err := os.ReadFile(previous.path)
	if !previous.exists {
		if os.IsNotExist(err) {
			return nil
		}
		return os.Remove(previous.path)
	}
	if err == nil && bytes.Equal(previous.data, current) {
		return nil
	}
	return writeOverrideAtomic(previous.path, previous.data, previous.mode)
}
