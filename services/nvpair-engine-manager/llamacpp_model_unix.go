// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func openLlamaModel(cache, value string) (*os.File, error) {
	if strings.Contains(value, ":Q4_K_M") {
		name, err := recordedLlamaModelPath(cache, value)
		if err != nil {
			return nil, err
		}
		value = name
	}
	root, err := filepath.Abs(cache)
	if cache == "" || err != nil {
		return nil, fmt.Errorf("llama.cpp model cache must be an existing directory")
	}
	// Resolve system aliases (e.g. /var on macOS) once. Never re-resolve the
	// validated model path: doing so could bless a swapped cache parent's target.
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve llama.cpp model cache: %w", err)
	}
	rel := value
	if filepath.IsAbs(value) {
		rel, err = filepath.Rel(root, value)
		if err != nil {
			return nil, fmt.Errorf("llama.cpp model must be inside the PAIR model cache")
		}
	}
	return openLlamaModelAt(root, rel)
}

// root is the canonical cache path, not a pathname to resolve again. Walk from
// / so a symlink swap in ANY ancestor fails rather than redirecting the open.
// Each successful open pins the next directory even if its name is then moved.
func openLlamaModelAt(root, rel string) (*os.File, error) {
	parts := strings.Split(rel, string(filepath.Separator))
	if !filepath.IsAbs(root) || filepath.IsAbs(rel) || rel == "" || !strings.EqualFold(filepath.Ext(rel), ".gguf") {
		return nil, fmt.Errorf("llama.cpp model must be a regular .gguf file inside the PAIR model cache")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || filepath.IsAbs(part) {
			return nil, fmt.Errorf("llama.cpp model must use a cache-relative path without parent traversal")
		}
	}
	if llamaSplitName.MatchString(rel) {
		return nil, fmt.Errorf("split/multi-file GGUF models are not supported yet; select a single-file GGUF")
	}
	dirFlags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	dirfd, err := unix.Open("/", dirFlags, 0)
	if err != nil {
		return nil, fmt.Errorf("open llama.cpp model cache root: %w", err)
	}
	defer func() { _ = unix.Close(dirfd) }()
	dirs := strings.Split(strings.TrimPrefix(root, string(filepath.Separator)), string(filepath.Separator))
	if root == string(filepath.Separator) {
		dirs = nil
	}
	dirs = append(dirs, parts[:len(parts)-1]...)
	for _, part := range dirs {
		next, err := unix.Openat(dirfd, part, dirFlags, 0)
		if err != nil {
			return nil, fmt.Errorf("llama.cpp model cache directory changed or is unavailable; select an in-cache model: %w", err)
		}
		_ = unix.Close(dirfd)
		dirfd = next
	}
	// Keep the parent's copy close-on-exec. ExtraFiles clears that flag only
	// on the child's copy; O_NONBLOCK avoids waiting on a substituted FIFO.
	fd, err := unix.Openat(dirfd, parts[len(parts)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open llama.cpp model without symlinks: %w", err)
	}
	file := os.NewFile(uintptr(fd), rel)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("llama.cpp model must be a regular .gguf file inside the PAIR model cache")
	}
	if len(parts) == 1 {
		// Pin the same cache directory used for the launch open and manifest.
		dup, err := unix.FcntlInt(uintptr(dirfd), unix.F_DUPFD_CLOEXEC, 0)
		if err != nil {
			_ = file.Close()
			return nil, err
		}
		dir := os.NewFile(uintptr(dup), root)
		err = validateLlamaRecordedFile(dir, rel, file)
		_ = dir.Close()
		if err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	if err := rejectSplitGGUF(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}
