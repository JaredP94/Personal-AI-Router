// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
)

func llamaCacheUnsupported() error {
	return fmt.Errorf("llama.cpp model cache operations are unsupported on Windows")
}
func openLlamaCache(string, bool) (*os.File, error)           { return nil, llamaCacheUnsupported() }
func llamaCacheOpen(*os.File, string, bool) (*os.File, error) { return nil, llamaCacheUnsupported() }
func llamaCacheRemove(*os.File, string) error                 { return llamaCacheUnsupported() }
func llamaCacheRename(*os.File, string, string) error         { return llamaCacheUnsupported() }
func llamaCacheCommit(*os.File, string, string) error         { return llamaCacheUnsupported() }
func llamaCacheIdentity(*os.File) string                      { return "" }
func llamaCacheMatch(*os.File, string, string, int64) error   { return llamaCacheUnsupported() }
func llamaCacheFree(*os.File) (uint64, error)                 { return 0, llamaCacheUnsupported() }
