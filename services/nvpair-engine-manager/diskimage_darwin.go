// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// attachTimeout bounds mounting a disk image, which is local and read-only.
const attachTimeout = 5 * time.Minute

const (
	hdiutilPath = "/usr/bin/hdiutil"
	dittoPath   = "/usr/bin/ditto"
	openPath    = "/usr/bin/open"
)

// installAppFromDiskImage copies di.App out of the disk image into di.Into,
// unless an app is already there, and then opens it when di.Open is set. An
// app already in place is the user's and is never replaced.
//
// identity is the file identity of the bundle this call put in place, taken
// from the staging copy before it was published, or "" when it placed nothing
// (an app was already there). The caller claims only that bundle.
func installAppFromDiskImage(ctx context.Context, image string, di *DiskImage) (identity string, err error) {
	dest := filepath.Join(expandPath(di.Into), di.App)
	if _, err := os.Lstat(dest); os.IsNotExist(err) {
		if identity, err = copyAppFromDiskImage(ctx, image, di.App, dest); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	if !di.Open {
		return identity, nil
	}
	if out, err := exec.CommandContext(ctx, openPath, "-g", dest).CombinedOutput(); err != nil {
		return identity, fmt.Errorf("open %s: %w: %s", dest, err, strings.TrimSpace(string(out)))
	}
	return identity, nil
}

// copyAppFromDiskImage mounts the image read-only and copies the app into a
// hidden sibling of dest, renamed into place only once complete, so neither a
// failure nor a cancellation leaves a partial app where a real one belongs.
// The mount is detached on every path, with a context of its own, because the
// caller's may already be cancelled.
//
// Attaching is not cancelled with the caller: hdiutil finishes attaching even
// after the client that asked is killed, so a cancelled attach would leave a
// mount behind that no cleanup could see. It runs to completion under its own
// bound, and the cancellation is honoured as soon as it returns.
func copyAppFromDiskImage(ctx context.Context, image, app, dest string) (string, error) {
	mnt, err := os.MkdirTemp("", "pair-dmg-")
	if err != nil {
		return "", err
	}
	defer func() {
		detachDiskImage(mnt)
		_ = os.Remove(mnt)
	}()
	attachCtx, cancelAttach := context.WithTimeout(context.Background(), attachTimeout)
	out, err := exec.CommandContext(attachCtx, hdiutilPath, "attach", "-nobrowse", "-readonly", "-noautoopen", "-mountpoint", mnt, image).CombinedOutput()
	cancelAttach()
	if err != nil {
		return "", fmt.Errorf("mount the disk image: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	src := filepath.Join(mnt, app)
	if info, err := os.Lstat(src); err != nil || !info.IsDir() {
		return "", fmt.Errorf("the disk image has no %s", app)
	}
	staging, err := os.MkdirTemp(filepath.Dir(dest), "."+app+".pair-")
	if err != nil {
		return "", fmt.Errorf("prepare %s: %w", filepath.Dir(dest), err)
	}
	placed := false
	defer func() {
		if !placed {
			_ = os.RemoveAll(staging)
		}
	}()
	if out, err := exec.CommandContext(ctx, dittoPath, src, staging).CombinedOutput(); err != nil {
		return "", fmt.Errorf("copy %s: %w: %s", app, err, strings.TrimSpace(string(out)))
	}
	// MkdirTemp made the staging directory private; the app is for everyone.
	if err := os.Chmod(staging, 0o755); err != nil {
		return "", err
	}
	if _, err := os.Lstat(dest); err == nil {
		return "", nil // an app arrived while copying; it is the user's, so leave it
	}
	// Rename keeps the bundle's identity, so the one taken here is the
	// bundle that lands at dest, captured before anyone else can touch it.
	identity := fileIdentity(staging)
	if identity == "" {
		return "", fmt.Errorf("cannot identify the copied %s", app)
	}
	if err := os.Rename(staging, dest); err != nil {
		return "", fmt.Errorf("place %s: %w", dest, err)
	}
	placed = true
	return identity, nil
}

func detachDiskImage(mnt string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if exec.CommandContext(ctx, hdiutilPath, "detach", "-quiet", mnt).Run() == nil {
		return
	}
	_ = exec.CommandContext(ctx, hdiutilPath, "detach", "-quiet", "-force", mnt).Run()
}
