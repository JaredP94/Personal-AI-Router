// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"nvpair-shared/jsonrpc"
)

func newStdioTransport() io.ReadWriteCloser {
	transport, err := jsonrpc.NewStdioTransport()
	if err != nil {
		panic("cannot initialize interruptible JSON-RPC stdio")
	}
	return transport
}
