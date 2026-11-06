// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package jsonrpc

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"
	"time"
)

func TestRelayInvalidParamsReportsSubmissionError(t *testing.T) {
	c, _ := leakCheckedPipe(t)
	peer := NewPeer(c)
	defer peer.Close()
	done := make(chan error, 1)
	if err := peer.RelayRequest("invalid", json.RawMessage(`{`), func(_ json.RawMessage, _ *RPCError, err error) { done <- err }); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("invalid params succeeded")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("submission error stranded relay callback")
	}
}

func TestRelayAdmissionOrdersWithNotifications(t *testing.T) {
	c, remote := leakCheckedPipe(t)
	peer := NewPeer(c)
	defer peer.Close()
	ignore := func(json.RawMessage, *RPCError, error) {}
	if err := peer.RelayRequest("first", nil, ignore); err != nil {
		t.Fatal(err)
	}
	prefix := make([]byte, 1)
	if _, err := io.ReadFull(remote, prefix); err != nil {
		t.Fatal(err)
	}
	if err := peer.RelayRequest("second", nil, ignore); err != nil {
		t.Fatal(err)
	}
	if err := peer.Notify("third", nil); err != nil {
		t.Fatal(err)
	}
	reader := readCodec(io.MultiReader(bytes.NewReader(prefix), remote))
	for _, want := range []string{"first", "second", "third"} {
		msg, err := reader.Read()
		if err != nil || msg.Method != want {
			t.Fatalf("want %s, got %+v, %v", want, msg, err)
		}
	}
}
