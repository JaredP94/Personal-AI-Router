// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package rpc

import (
	"context"
	"errors"
	"io"
	"nvpair-shared/jsonrpc"
	"strings"
	"testing"
	"time"
)

func TestClientMismatchStopsOpenInput(t *testing.T) {
	held, release := io.Pipe()
	defer held.Close()
	defer release.Close()
	c := NewClient(io.MultiReader(strings.NewReader(`{"jsonrpc":"2.0","method":"ready"}
{"jsonrpc":"2.0","method":"trailing","epoch":"x","seq":1}
`), held), io.Discard)
	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()
	select {
	case err := <-done:
		var mismatch *jsonrpc.ProtocolMismatchError
		if !errors.As(err, &mismatch) {
			t.Fatalf("not mismatch: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("client remained alive")
	}
	if msg, ok := <-c.Notifications(); ok {
		t.Fatalf("trailing delivered: %v", msg)
	}
}
