// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Open without following a last-component link. The parent descriptor stays
// close-on-exec; exec.Cmd.ExtraFiles clears that flag only on the child's copy.
func openLlamaModel(cache, value string) (*os.File, error) {
	path, err := llamaModelPath(cache, value)
	if err != nil {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open verified llama.cpp model: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	fail := func() (*os.File, error) {
		_ = file.Close()
		return nil, fmt.Errorf("llama.cpp model or cache changed before launch; select a regular .gguf file inside the PAIR model cache")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fail()
	}
	// Re-resolve AFTER opening and compare device/inode, not just the spelling.
	resolved, err := llamaModelPath(cache, value)
	if err != nil {
		return fail()
	}
	current, err := os.Stat(resolved)
	if err != nil || !os.SameFile(info, current) {
		return fail()
	}
	// The cache itself must not redirect to an unrelated directory. Resolve
	// only its parent to permit system links such as macOS /var -> /private/var.
	absolute, err := filepath.Abs(cache)
	if err != nil {
		return fail()
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return fail()
	}
	root, err := filepath.EvalSymlinks(absolute)
	if err != nil || root != filepath.Join(parent, filepath.Base(absolute)) {
		return fail()
	}
	if err := rejectSplitGGUF(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}
