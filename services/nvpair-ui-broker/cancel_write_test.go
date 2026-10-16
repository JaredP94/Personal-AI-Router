// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"
)

type blockedClientTransport struct {
	io.Reader
	method  string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	ready   chan struct{}
}

func (w *blockedClientTransport) Close() error { return nil }
func (w *blockedClientTransport) Write(p []byte) (int, error) {
	var msg Message
	if err := json.Unmarshal(p, &msg); err != nil {
		return 0, err
	}
	if msg.Method == w.method || (w.method == "response" && msg.IsResponse()) {
		w.once.Do(func() { close(w.entered) })
		<-w.release
	}
	if msg.Method == "app:ready" {
		close(w.ready)
	}
	return len(p), nil
}
func TestServeCancellationBoundsAllClientWrites(t *testing.T) {
	for _, method := range []string{"app:ready", "response"} {
		t.Run(method, func(t *testing.T) {
			for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "APPDATA", "LOCALAPPDATA"} {
				t.Setenv(key, t.TempDir())
			}
			input, send := io.Pipe()
			defer input.Close()
			defer send.Close()
			transport := &blockedClientTransport{Reader: input, method: method, entered: make(chan struct{}), release: make(chan struct{}), ready: make(chan struct{})}
			defer close(transport.release)
			b := NewBroker(NewCodec(transport), workerPaths{scanner: newFakeScannerPath(t)})
			b.closeTransport = transport.Close
			b.heartbeatInterval = time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- b.Serve(ctx) }()
			if method == "response" {
				select {
				case <-transport.ready:
				case <-time.After(2 * time.Second):
					t.Fatal("not ready")
				}
				go func() { _, _ = send.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"broker:resync"}` + "\n")) }()
			}
			select {
			case <-transport.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("target write not reached")
			}
			scanner := b.getScanner()
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("Serve parked on canceled write")
			}
			select {
			case <-scanner.Done():
			default:
				t.Fatal("scanner not reaped before Serve returned")
			}
		})
	}
}
