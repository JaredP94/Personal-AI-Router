// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

// Pin the canonical directory using the same component-by-component no-follow
// walk as model launch. All later mutations are relative to this descriptor.
func openLlamaCache(root string, create bool) (*os.File, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	// Only canonical system aliases are normalized. Never resolve arbitrary
	// parent links: that would bless a swapped ancestor before the walk.
	if runtime.GOOS == "darwin" {
		for _, alias := range []string{"/tmp", "/var"} {
			if root == alias || strings.HasPrefix(root, alias+"/") {
				root = "/private" + root
				break
			}
		}
	}
	parent := filepath.Dir(root)
	dir, err := openLlamaCacheAt(parent)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	name := filepath.Base(root)
	if create {
		if err := unix.Mkdirat(int(dir.Fd()), name, 0700); err != nil && err != unix.EEXIST {
			return nil, err
		}
	}
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), filepath.Join(parent, name)), nil
}
func openLlamaCacheAt(root string) (*os.File, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, fmt.Errorf("invalid cache root")
	}
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Open("/", flags, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(root, "/"), "/") {
		if part == "" {
			continue
		}
		next, err := unix.Openat(fd, part, flags, 0)
		_ = unix.Close(fd)
		if err != nil {
			return nil, fmt.Errorf("cache directory changed or unavailable")
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), root), nil
}
func llamaCacheOpen(dir *os.File, name string, create bool) (*os.File, error) {
	if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return nil, fmt.Errorf("invalid cache file")
	}
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	if create {
		flags = unix.O_RDWR | unix.O_CREAT | unix.O_EXCL | unix.O_NOFOLLOW | unix.O_CLOEXEC
	}
	fd, err := unix.Openat(int(dir.Fd()), name, flags, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("cache entry is not a regular file")
	}
	return f, nil
}
func llamaCacheRemove(dir *os.File, name string) error { return unix.Unlinkat(int(dir.Fd()), name, 0) }
func llamaCacheRename(dir *os.File, from, to string) error {
	return unix.Renameat(int(dir.Fd()), from, int(dir.Fd()), to)
}
func llamaCacheIdentity(f *os.File) string {
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino)
}

// The cache belongs to the user; swaps require another same-user process.
// fstatat never follows a substituted link. macOS has no rename/unlink by fd:
// callers must check at the last possible point and fail closed on mismatch.
func llamaCacheMatch(dir *os.File, name, identity string, size int64) error {
	var st unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if identity == "" || fmt.Sprintf("%d:%d", st.Dev, st.Ino) != identity || st.Mode&unix.S_IFMT != unix.S_IFREG || (size >= 0 && st.Size != size) {
		return fmt.Errorf("model cache entry changed; refusing unverified file")
	}
	return nil
}

func llamaCacheFree(dir *os.File) (uint64, error) {
	var st unix.Statfs_t
	err := unix.Fstatfs(int(dir.Fd()), &st)
	return uint64(st.Bavail) * uint64(st.Bsize), err
}
