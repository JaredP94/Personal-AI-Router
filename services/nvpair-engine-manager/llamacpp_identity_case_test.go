// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLlamaRecordedIdentityCannotBeBypassedByFilenameCase(t *testing.T) {
	ex, cache := llamaDownloadExecutor(t, nil, nil)
	if err := pullLlamaTest(ex, "case-swap"); err != nil {
		t.Fatal(err)
	}
	name := llamaOwnedFilename(llamaTestID)
	path := filepath.Join(cache, name)
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, llamaTestBytes(), 0600); err != nil {
		t.Fatal(err)
	}
	upper := filepath.Join(cache, strings.ToUpper(name))
	// Default macOS filesystems already alias this spelling. On Linux make the
	// equivalent alias explicitly so the identity guard is exercised everywhere.
	if err := os.Link(path, upper); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
	if _, err := llamaModelPath(cache, upper); err == nil {
		t.Error("case alias bypassed selection identity")
	}
	if f, err := openLlamaModel(cache, upper); err == nil {
		f.Close()
		t.Error("case alias bypassed launch identity")
	}
	if !llamaManagedFilename(strings.ToUpper(name)) {
		t.Error("case alias escaped reserved download namespace")
	}
}
