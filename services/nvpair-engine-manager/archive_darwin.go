// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
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
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("signature root is not a directory")
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	for _, name := range spec.Required {
		if !safeArchivePath(name) {
			return fmt.Errorf("invalid required archive path")
		}
		path := filepath.Join(root, filepath.FromSlash(name))
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || !pathWithinRoot(root, resolved) {
			return fmt.Errorf("required archive file %s is missing or unsafe", name)
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("required archive file %s is missing or empty", name)
		}
	}
	target := filepath.Join(root, filepath.FromSlash(spec.Codesign))
	info, err = os.Stat(target)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return fmt.Errorf("signature target %s is missing or not executable", spec.Codesign)
	}
	mach, err := archiveMachO(target)
	if err != nil {
		return err
	}
	if !mach {
		return fmt.Errorf("signature target %s is not Mach-O", spec.Codesign)
	}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil || !pathWithinRoot(root, resolved) {
				return fmt.Errorf("unsafe installed archive link %s", path)
			}
			return nil // The contained target is visited as a regular file.
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported installed archive file %s", path)
		}
		mach, err := archiveMachO(path)
		if err != nil {
			return err
		}
		if !mach {
			return nil
		}
		if err := exec.CommandContext(ctx, tool, "--verify", "--strict", "--all-architectures", path).Run(); err != nil {
			return fmt.Errorf("cannot verify the existing signature of %s (codesign is required): %w", path, err)
		}
		return nil
	})
}

// Thin 32/64-bit and fat 32/64-bit Mach-O, in both byte orders. File names
// and execute bits do not identify code: adjacent dylibs often have neither.
func archiveMachO(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return false, nil
		}
		return false, err
	}
	switch binary.BigEndian.Uint32(magic[:]) {
	case 0xfeedface, 0xcefaedfe, 0xfeedfacf, 0xcffaedfe, 0xcafebabe, 0xbebafeca, 0xcafebabf, 0xbfbafeca:
		return true, nil
	}
	return false, nil
}
