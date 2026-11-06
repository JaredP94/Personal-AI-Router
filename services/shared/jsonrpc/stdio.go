// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package jsonrpc

import (
	"errors"
	"io"
	"os"
)

type stdioStream struct{ reader, writer *os.File }

func (s *stdioStream) Read(p []byte) (int, error)  { return s.reader.Read(p) }
func (s *stdioStream) Write(p []byte) (int, error) { return s.writer.Write(p) }
func (s *stdioStream) Close() error                { return errors.Join(s.writer.Close(), s.reader.Close()) }

// NewStdioTransport takes ownership of stdin/stdout. On Unix inherited pipes
// must be registered with the Go poller so Close can interrupt a full-pipe write.
// Do not use os.Stdin/os.Stdout after this call (stderr is unaffected).
func NewStdioTransport() (io.ReadWriteCloser, error) {
	reader, err := interruptibleStdioFile(os.Stdin)
	if err != nil {
		return nil, err
	}
	writer, err := interruptibleStdioFile(os.Stdout)
	if err != nil {
		_ = reader.Close()
		return nil, err
	}
	return &stdioStream{reader: reader, writer: writer}, nil
}
