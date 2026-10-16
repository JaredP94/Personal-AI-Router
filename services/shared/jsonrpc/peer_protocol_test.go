// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package jsonrpc

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

type heldPeerStream struct {
	io.Reader
	io.Writer
}

func TestPeerMismatchTerminatesOpenInput(t *testing.T) {
	held, release := io.Pipe()
	defer held.Close()
	defer release.Close()
	p := NewPeer(NewCodec(heldPeerStream{io.MultiReader(strings.NewReader(`{"jsonrpc":"2.0","method":"ready"}
{"jsonrpc":"2.0","id":1,"method":"trailing"}
`), held), &bytes.Buffer{}}))
	called := false
	done := make(chan struct{})
	go func() { p.Serve(func(*Message) { called = true }, nil); close(done) }()
	select {
	case <-done:
		if called {
			t.Fatal("trailing dispatched")
		}
	case <-time.After(time.Second):
		t.Fatal("pump stayed alive")
	}
	select {
	case <-p.Done():
	default:
		t.Fatal("pending calls not closed")
	}
}
