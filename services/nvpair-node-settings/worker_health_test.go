// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
)

// TestWorkerHealth catches a missing health dispatch, incorrect correlation or
// payload, and a response deferred until after dispatch returns.
func TestWorkerHealth(t *testing.T) {
	for _, rawID := range []string{"7", `"health-7"`} {
		t.Run(rawID, func(t *testing.T) {
			var out bytes.Buffer
			m := &Manager{codec: NewCodec(&out)}
			id := json.RawMessage(rawID)
			msg := &Message{JSONRPC: "2.0", ID: &id, Method: "worker/health"}
			m.handleMessage(msg)
			output := out.String()

			var response Message
			if err := json.Unmarshal([]byte(output), &response); err != nil {
				t.Fatalf("dispatch did not respond inline: %q (%v)", output, err)
			}
			if response.JSONRPC != "2.0" || !response.IsResponse() || response.ID == nil || string(*response.ID) != rawID {
				t.Fatalf("incorrect response envelope: %s", output)
			}
			if response.Error != nil || string(response.Result) != `{"ok":true}` {
				t.Fatalf("worker/health result = %s, error = %v; want {\"ok\":true}", response.Result, response.Error)
			}
		})
	}
}

// TestWorkerHealthWriteError catches a failed response being silently discarded.
// The diagnostic must be emitted inline without request or response payloads.
func TestWorkerHealthWriteError(t *testing.T) {
	var diagnostics bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&diagnostics, nil)))
	defer slog.SetDefault(previous)

	local, remote := net.Pipe()
	defer local.Close()
	if err := remote.Close(); err != nil {
		t.Fatal(err)
	}
	m := &Manager{codec: NewCodec(local)}
	id := json.RawMessage(`"private-health-id"`)
	msg := &Message{
		JSONRPC: "2.0", ID: &id, Method: "worker/health",
		Params: json.RawMessage(`{"private":"health-params"}`),
	}
	m.handleMessage(msg)

	output := diagnostics.String()
	if !strings.Contains(output, "level=ERROR") ||
		!strings.Contains(output, "worker/health") ||
		!strings.Contains(output, io.ErrClosedPipe.Error()) {
		t.Fatalf("health write error was not surfaced inline: %q", output)
	}
	for _, payload := range []string{"private-health-id", "health-params", `"ok"`} {
		if strings.Contains(output, payload) {
			t.Fatalf("health diagnostic contains payload %q: %s", payload, output)
		}
	}
}
