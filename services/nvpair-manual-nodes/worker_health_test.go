// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
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
