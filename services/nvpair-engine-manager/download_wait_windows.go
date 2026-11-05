// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

// Windows already holds a separate launcher handle and owns its job, so its
// existing concurrent Wait cannot release the identity used by cleanup.
func watchDownloadExit(w *downloadWait) error {
	w.reap()
	go func() { <-w.finished; close(w.exited) }()
	return nil
}
