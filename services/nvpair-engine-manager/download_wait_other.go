// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !darwin && !linux && !windows

package main

// Unsupported targets have no non-reaping exit watcher. Use ordinary Wait;
// this is a build fallback, not supported detached-downloader supervision.
func watchDownloadExit(w *downloadWait) error {
	w.reap()
	go func() { <-w.finished; close(w.exited) }()
	return nil
}
