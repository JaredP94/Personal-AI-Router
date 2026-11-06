// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package jsonrpc

import "os"

// os.File.Close cancels pending pipe I/O on Windows.
func interruptibleStdioFile(f *os.File) (*os.File, error) { return f, nil }
