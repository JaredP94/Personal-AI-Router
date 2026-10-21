// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nvpair-shared/telemetry"
)

// progressNotifications decodes the live progress frames the proxy wrote.
func progressNotifications(t *testing.T, rec *prRec) []ProgressFrame {
	t.Helper()
	rec.mu.Lock()
	raw := string(rec.b)
	rec.mu.Unlock()
	var frames []ProgressFrame
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var note struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &note); err != nil {
			t.Fatalf("the proxy wrote a line that is not JSON: %q", line)
		}
		if note.Method != progressMethod {
			continue
		}
		var frame ProgressFrame
		if err := json.Unmarshal(note.Params, &frame); err != nil {
			t.Fatalf("a progress frame is not readable: %v (%s)", err, note.Params)
		}
		frames = append(frames, frame)
	}
	return frames
}

// TestTelemetryReaderCountsLiveProgress drives the reader with a known SSE
// stream and checks that its live counters are exact: the content chunks are
// the chunks that carried text, the bytes are the bytes the engine sent, and
// the first-byte and first-content instants are recorded.
func TestTelemetryReaderCountsLiveProgress(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\" there\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: [DONE]\n\n"

	session := telemetry.NewManager("nvpair-lmstudio-proxy-test", telemetry.OTLPExporter).Session()
	reader := newTelemetryBodyReader(io.NopCloser(strings.NewReader(stream)), nil, time.Now(), time.Now(), session)
	buf := make([]byte, 13)
	for {
		if _, err := reader.Read(buf); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
	}

	prog := reader.Progress()
	if prog.Chunks != 2 {
		t.Fatalf("content chunks = %d, want 2 (the role-only chunk carries no text)", prog.Chunks)
	}
	if prog.Bytes != int64(len(stream)) {
		t.Fatalf("response bytes = %d, want %d (every byte the engine sent)", prog.Bytes, len(stream))
	}
	if !prog.HasByte || !prog.HasContent {
		t.Fatal("the first byte and the first content chunk were not both recorded")
	}
	if prog.TTFBMs < 0 {
		t.Fatalf("time to first byte = %d ms, want a measured non-negative duration", prog.TTFBMs)
	}
	if prog.TTFCMs < prog.TTFBMs {
		t.Fatalf("time to first content = %d ms, want it at or after the first byte (%d ms)", prog.TTFCMs, prog.TTFBMs)
	}
}

// TestInFlightRequestEmitsOneProgressFrame: an in-flight request publishes a
// live progress frame, and a burst of chunks inside one window produces one
// frame — not one per chunk.
func TestInFlightRequestEmitsOneProgressFrame(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// The whole burst in one write, so the reader sees it in one read
		// and the frame's counts are the engine's counts.
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{\"content\":\"c\"}}]}\n\n"+
			"data: [DONE]\n\n")
	}))
	defer engine.Close()

	rec := &prRec{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "serving-node", engine.URL, "llama"))
	p := NewProxy(NewCodec(rec), disc, 1234)

	p.handleHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"llama"}`)))

	frames := progressNotifications(t, rec)
	if len(frames) != 1 {
		t.Fatalf("progress frames = %d, want 1 for a burst of chunks inside one window", len(frames))
	}
	frame := frames[0]
	if frame.JobID == "" {
		t.Fatal("the progress frame carries no job id")
	}
	if frame.NodeID != "serving-node" {
		t.Fatalf("the progress frame names node %q, want the serving node", frame.NodeID)
	}
	// The engine is part of the job's identity: every proxy counts its own
	// requests from 1, so an lmstudio job "1" and another engine's job "1" on
	// the same node are two different jobs.
	if frame.Engine != workloadEngine {
		t.Fatalf("the progress frame names engine %q, want %q", frame.Engine, workloadEngine)
	}
	// The run nonce says which run the job id belongs to, so a proxy that
	// restarted and counts from 1 again never takes the old job for the
	// new one.
	if frame.RunID != p.runID {
		t.Fatalf("the progress frame carries run id %q, want the proxy's run nonce %q", frame.RunID, p.runID)
	}
	if frame.Chunks == nil || *frame.Chunks != 3 {
		t.Fatalf("the progress frame reports chunks %v, want the 3 content chunks the engine sent", frame.Chunks)
	}
	if frame.Bytes == nil || *frame.Bytes <= 0 {
		t.Fatalf("the progress frame reports bytes %v, want the bytes received", frame.Bytes)
	}
	if frame.ElapsedMs == nil {
		t.Fatal("the progress frame reports no elapsed time")
	}
	// A chunk is not a token: the frame must carry no token count and no
	// rate derived from one.
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"oken", "PerSecond", "perSecond"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("the progress frame carries an estimated number %q: %s", forbidden, raw)
		}
	}
}
