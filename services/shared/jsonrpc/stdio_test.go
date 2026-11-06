// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package jsonrpc

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestInheritedStdioPipeDeadlineJoinsWriter(t *testing.T) {
	baseline := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, baseline) })
	input, inputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close(); _ = inputWriter.Close() })
	outputReader, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = outputReader.Close(); _ = output.Close() })
	_ = output.Fd() // reproduce an inherited, blocking Unix stdout descriptor
	pollable, err := interruptibleStdioFile(output)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCodec(&stdioStream{reader: input, writer: pollable})
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Notify("large-snapshot", strings.Repeat("x", 1<<20)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
	case <-time.After(WriteTimeout + time.Second):
		t.Fatal("inherited full pipe defeated deadline")
	}
	if !errors.Is(c.Err(), ErrWriteTimeout) {
		t.Fatalf("terminal reason: %v", c.Err())
	}
}
