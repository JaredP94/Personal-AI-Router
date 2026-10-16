// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package applog

import (
	"bytes"
	"encoding/json"
	"io"
	"nvpair-shared/jsonrpc"
	"strings"
	"testing"
	"time"
)

type heldStdin struct {
	io.Reader
	io.Closer
}

func TestStdinRPCRejectsMismatch(t *testing.T) {
	for _, frame := range []string{
		`{"jsonrpc":"2.0","method":"shutdown"}`,
		`{"jsonrpc":"2.0","method":"shutdown","epoch":"x"}`,
		`{"jsonrpc":"2.0","method":"shutdown","seq":1}`,
		`{"jsonrpc":"2.0","id":1,"method":"shutdown","epoch":"x","seq":1}`,
		`{"jsonrpc":"2.0","id":1,"result":{},"epoch":"x","seq":1}`,
	} {
		t.Run(frame, func(t *testing.T) {
			held, release := io.Pipe()
			defer held.Close()
			defer release.Close()
			done := make(chan struct{})
			called := false
			go serveStdinRPC(heldStdin{io.MultiReader(strings.NewReader(frame+"\n"+`{"jsonrpc":"2.0","id":2,"method":"shutdown"}`+"\n"), held), held}, &bytes.Buffer{}, func(StdinMessage) { called = true }, func() { close(done) })
			select {
			case <-done:
				if called {
					t.Fatal("invalid/trailing control ran")
				}
			case <-time.After(time.Second):
				t.Fatal("stdin reader did not terminate")
			}
		})
	}
}

func TestStdinRPCRecoversMalformedFrameAndPreservesRequestID(t *testing.T) {
	var input, output bytes.Buffer
	input.WriteString("{not json}\n")
	codec := jsonrpc.NewCodec(stdinReadWriter{strings.NewReader(""), &input})
	if err := codec.Notify("custom-control", nil); err != nil {
		t.Fatal(err)
	}
	id := json.RawMessage(`"control-request"`)
	if err := codec.Request(&id, SetLevelMethod, json.RawMessage(`{"level":"invalid"}`)); err != nil {
		t.Fatal(err)
	}
	calls, closes := 0, 0
	serveStdinRPC(io.NopCloser(&input), &output, func(msg StdinMessage) {
		calls++
		if msg.Method != "custom-control" {
			t.Fatal("built-in request escaped handler")
		}
	}, func() { closes++ })
	if calls != 1 || closes != 1 {
		t.Fatalf("callbacks=%d closes=%d", calls, closes)
	}
	reply, err := jsonrpc.NewCodec(stdinReadWriter{&output, io.Discard}).Read()
	if err != nil || reply.ID == nil || string(*reply.ID) != string(id) || reply.Error == nil {
		t.Fatalf("built-in reply lost ID/validation: %v %v", reply, err)
	}
}
