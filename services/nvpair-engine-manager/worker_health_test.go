// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
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
