// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func swapArchiveDirectories(stage, target string) error {
	return unix.RenameatxNp(unix.AT_FDCWD, stage, unix.AT_FDCWD, target, unix.RENAME_SWAP)
}

func renameArchiveNew(stage, target string) error {
	return unix.RenameatxNp(unix.AT_FDCWD, stage, unix.AT_FDCWD, target, unix.RENAME_EXCL)
}

func rejectArchiveQuarantine(p string) error {
	_, err := unix.Lgetxattr(p, "com.apple.quarantine", nil)
	if err == nil {
		return fmt.Errorf("cannot install %s: it carries com.apple.quarantine; the file was left unchanged", p)
	}
	if !errors.Is(err, unix.ENOATTR) {
		return fmt.Errorf("cannot check quarantine for %s: %w", p, err)
	}
	return nil
}

func verifyArchivePlatform(ctx context.Context, root string, spec *ArchiveInstall) error {
	if err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return rejectArchiveQuarantine(p)
	}); err != nil {
		return err
	}
	return verifyArchiveSignature(ctx, root, spec, "/usr/bin/codesign")
}

// argv only; never re-sign, execute downloaded code or alter quarantine.
// The fixed system tool is required, not a PATH-provided replacement.
func verifyArchiveSignature(ctx context.Context, root string, spec *ArchiveInstall, tool string) error {
	if !safeArchivePath(spec.Codesign) {
		return fmt.Errorf("invalid signature target")
	}
	target := filepath.Join(root, filepath.FromSlash(spec.Codesign))
	info, err := os.Stat(target)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return fmt.Errorf("signature target %s is missing or not executable", spec.Codesign)
	}
	if err := exec.CommandContext(ctx, tool, "--verify", "--strict", target).Run(); err != nil {
		return fmt.Errorf("cannot verify the existing signature of %s (codesign is required): %w", spec.Codesign, err)
	}
	return nil
}
