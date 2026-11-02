// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func streamingPeer(t *testing.T, body string) *remoteClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return &remoteClient{http: srv.Client(), base: srv.URL}
}

// A peer cannot make this node buffer an unbounded frame: one line past the
// per-frame cap fails the operation.
func TestRemoteStreamRejectsAnOversizedFrame(t *testing.T) {
	huge := `{"type":"progress","message":"` + strings.Repeat("x", remoteStreamFrameBytes) + `"}` + "\n"
	c := streamingPeer(t, huge+`{"type":"result"}`+"\n")
	_, err := c.stream(context.Background(), "/v1/control/install", map[string]string{}, nil)
	if err == nil || !strings.Contains(err.Error(), "read frame") {
		t.Fatalf("err = %v, want a read-frame error", err)
	}
}

// The cap is per frame, not per stream: a long operation reports progress for
// as long as it runs.
func TestRemoteStreamAcceptsManySmallFrames(t *testing.T) {
	var body strings.Builder
	frame := `{"type":"progress","message":"` + strings.Repeat("x", 4096) + `"}` + "\n"
	for body.Len() < 2*remoteStreamFrameBytes {
		body.WriteString(frame)
	}
	body.WriteString(`{"type":"result","stage":"done"}` + "\n")
	progress := 0
	c := streamingPeer(t, body.String())
	result, err := c.stream(context.Background(), "/v1/control/install", map[string]string{}, func(streamFrame) { progress++ })
	if err != nil || result.Stage != "done" || progress == 0 {
		t.Fatalf("result = %+v, err = %v, progress = %d; want done after progress", result, err, progress)
	}
}
