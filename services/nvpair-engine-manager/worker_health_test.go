// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

// TestWorkerHealth catches a missing health dispatch, incorrect correlation or
// payload, and a response deferred until after dispatch returns.
func TestWorkerHealth(t *testing.T) {
	for _, rawID := range []string{"7", `"health-7"`} {
		t.Run(rawID, func(t *testing.T) {
			var out bytes.Buffer
			ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
			st, err := ex.state("fake")
			if err != nil {
				t.Fatal(err)
			}
			// A long engine operation may hold these locks throughout a health probe.
			st.opMu.Lock()
			defer st.opMu.Unlock()
			st.mu.Lock()
			defer st.mu.Unlock()
			ex.mu.Lock()
			defer ex.mu.Unlock()
			m := &Manager{codec: NewCodec(&out), exec: ex}
			id := json.RawMessage(rawID)
			msg := &Message{JSONRPC: "2.0", ID: &id, Method: "worker/health"}
			done := make(chan string, 1)
			go func() {
				m.handleMessage(context.Background(), msg)
				done <- out.String()
			}()
			var output string
			select {
			case output = <-done:
			case <-time.After(time.Second):
				t.Fatal("worker/health blocked behind an engine-operation lock")
			}

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
	m.handleMessage(context.Background(), msg)

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
