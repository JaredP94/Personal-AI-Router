// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"testing"

	"nvpair-shared/applog"
	"nvpair-shared/jsonrpc"
)

// Node-info uses StdinRPC as its control loop. Health must be answered there,
// not forwarded to the identity/demand handlers or silently dropped.
func TestWorkerHealth(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer writer.Close()
	oldStdin := os.Stdin
	os.Stdin = input
	defer func() { os.Stdin = oldStdin }()
	for _, frame := range []string{
		`{"jsonrpc":"2.0","id":7,"method":"worker/health"}`,
		`{"jsonrpc":"2.0","id":"health-7","method":"worker/health"}`,
	} {
		if _, err := writer.WriteString(frame + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	var identity clusterIdentity
	var collector statsCollector
	forwarded := false
	closed := false
	applog.StdinRPC(applog.NewNotifier(&out), func(msg applog.StdinMessage) {
		forwarded = true
		handleClusterIdentity(msg, &identity)
		handleDemand(msg, &collector)
	}, func() { closed = true })
	if forwarded || !closed {
		t.Fatalf("health forwarded = %v, stdin closed = %v", forwarded, closed)
	}
	decoder := json.NewDecoder(&out)
	for _, rawID := range []string{"7", `"health-7"`} {
		var response jsonrpc.Message
		if err := decoder.Decode(&response); err != nil {
			t.Fatalf("missing health response: %v", err)
		}
		if response.JSONRPC != "2.0" || !response.IsResponse() || response.ID == nil || string(*response.ID) != rawID {
			t.Fatalf("incorrect response envelope: %+v", response)
		}
		if response.Error != nil || string(response.Result) != `{"ok":true}` {
			t.Fatalf("worker/health result = %s, error = %v; want {\"ok\":true}", response.Result, response.Error)
		}
	}
	var extra jsonrpc.Message
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("unexpected trailing output: %+v (error = %v)", extra, err)
	}
}
