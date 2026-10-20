// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type terminalSource struct {
	io.Reader
	io.Writer
}
type terminalControl struct{ closed chan struct{} }

func (c *terminalControl) Write(p []byte) (int, error) { return len(p), nil }
func (c *terminalControl) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}

func TestRawWorkerReadersRejectMismatch(t *testing.T) {
	for _, frame := range []string{
		`{"jsonrpc":"2.0","method":"ready"}`,
		`{"jsonrpc":"2.0","method":"ready","epoch":"x"}`,
		`{"jsonrpc":"2.0","method":"ready","seq":1}`,
		`{"jsonrpc":"2.0","id":1,"method":"ready","epoch":"x","seq":1}`,
		`{"jsonrpc":"2.0","id":1,"result":{},"epoch":"x","seq":1}`,
	} {
		for _, kind := range []string{"nodeinfo", "workload"} {
			t.Run(kind+frame, func(t *testing.T) {
				held, release := io.Pipe()
				defer held.Close()
				defer release.Close()
				input := io.MultiReader(strings.NewReader(frame+"\n"+`{"jsonrpc":"2.0","method":"trailing","epoch":"x","seq":2}`+"\n"), held)
				done := make(chan struct{})
				calls := 0
				cb := func(string, json.RawMessage) { calls++ }
				go func() {
					defer close(done)
					if kind == "nodeinfo" {
						drainNodeInfoStdout(input, cb)
					} else {
						c := &terminalControl{make(chan struct{})}
						m := &workloadManagerProcess{stdin: c, onNotify: cb, peer: NewPeer(NewCodec(readWriter{input, io.Discard}))}
						m.readMessages()
						select {
						case <-c.closed:
						default:
							t.Error("control pipe not closed")
						}
					}
				}()
				select {
				case <-done:
					if calls != 0 {
						t.Fatal("invalid/trailing callback dispatched")
					}
				case <-time.After(time.Second):
					t.Fatal("reader did not terminate")
				}
			})
		}
	}
}

func TestWorkerMismatchClosesRealProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX isolated worker fixture")
	}
	path := filepath.Join(t.TempDir(), "v1-worker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' '{\"jsonrpc\":\"2.0\",\"method\":\"ready\"}'\ncat >/dev/null\n"), 0700); err != nil {
		t.Fatal(err)
	}
	var diagnostics bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&diagnostics, nil)))
	defer slog.SetDefault(previous)
	w, err := startRPCWorker("v1-fixture", path, nil, func(string, json.RawMessage) { t.Error("invalid notification delivered") })
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	select {
	case <-w.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("worker stdin remained open; supervisor did not see process exit")
	}
	if w.cmd.ProcessState == nil || !w.cmd.ProcessState.Exited() {
		t.Fatal("worker not reaped")
	}
	// Done is published after Wait, which follows read-pump diagnostics.
	if !strings.Contains(diagnostics.String(), "protocol mismatch") {
		t.Fatalf("missing named diagnostic: %s", diagnostics.String())
	}
}

func TestWorkloadReaderRecoversMalformedAndDispatchesOnlyValidNotifications(t *testing.T) {
	var input bytes.Buffer
	input.WriteString("{not json}\n")
	codec := NewCodec(writeOnlyReadWriter{&input})
	_ = codec.Notify("ready", nil)
	input.WriteString(`{"jsonrpc":"2.0","id":1,"method":"ignored-request"}` + "\n")
	control := &terminalControl{make(chan struct{})}
	calls := 0
	m := &workloadManagerProcess{stdin: control, peer: NewPeer(NewCodec(readWriter{&input, io.Discard})), onNotify: func(method string, _ json.RawMessage) {
		calls++
		if method != "ready" {
			t.Fatal("request dispatched as notification")
		}
	}}
	m.readMessages()
	if calls != 1 {
		t.Fatalf("valid notification not delivered: %d", calls)
	}
}
