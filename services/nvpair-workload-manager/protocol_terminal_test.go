// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"nvpair-shared/jsonrpc"
	"strings"
	"testing"
	"time"
)

type terminalTestStream struct {
	io.Reader
	io.Writer
}

func TestProtocolMismatchStopsOpenReadLoop(t *testing.T) {
	for _, frame := range []string{
		`{"jsonrpc":"2.0","method":"ready"}`,
		`{"jsonrpc":"2.0","method":"ready","epoch":"x"}`,
		`{"jsonrpc":"2.0","method":"ready","seq":1}`,
		`{"jsonrpc":"2.0","id":1,"method":"log/set-level","epoch":"x","seq":1}`,
		`{"jsonrpc":"2.0","id":1,"result":{},"epoch":"x","seq":1}`,
	} {
		t.Run(frame, func(t *testing.T) {
			held, release := io.Pipe()
			defer held.Close()
			defer release.Close()
			var out bytes.Buffer
			input := io.MultiReader(strings.NewReader(frame+"\n"+`{"jsonrpc":"2.0","id":2,"method":"log/set-level","params":{"level":"invalid"}}`+"\n"), held)
			m := &Manager{codec: NewCodec(terminalTestStream{input, &out}), cancel: func() {}}
			done := make(chan error, 1)
			go func() { done <- m.readLoop(context.Background()) }()
			select {
			case err := <-done:
				var mismatch *jsonrpc.ProtocolMismatchError
				if !errors.As(err, &mismatch) {
					t.Fatalf("expected named mismatch, got %v", err)
				}
				if out.Len() != 0 {
					t.Fatal("trailing command executed")
				}
			case <-time.After(time.Second):
				t.Fatal("read loop remained alive after mismatch with input held open")
			}
		})
	}
}
