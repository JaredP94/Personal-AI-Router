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
func progressNotifications(t *testing.T, rec *recRW) []ProgressFrame {
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

// TestTelemetryReaderCountsLiveProgress drives the reader with a known
// Ollama stream and checks that its live counters are exact: the content
// chunks are the chunks that carried text, the bytes are the bytes the
// engine sent, and the first-byte and first-content instants are recorded.
func TestTelemetryReaderCountsLiveProgress(t *testing.T) {
	stream := `{"model":"m","response":"Hello","done":false}` + "\n" +
		`{"model":"m","response":" there","done":false}` + "\n" +
		`{"model":"m","response":"","done":false}` + "\n" +
		`{"model":"m","done":true,"eval_count":7}` + "\n"

	session := telemetry.NewManager("nvpair-ollama-proxy-test", telemetry.OTLPExporter).Session()
	reader := newTelemetryBodyReader(io.NopCloser(strings.NewReader(stream)), nil, time.Now(), time.Now(), session)
	buf := make([]byte, 11)
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
		t.Fatalf("content chunks = %d, want 2 (the empty and the terminal chunk carry no text)", prog.Chunks)
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
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		// The whole burst in one write, so the reader sees it in one read
		// and the frame's counts are the engine's counts.
		_, _ = io.WriteString(w, `{"model":"m","response":"a","done":false}`+"\n"+
			`{"model":"m","response":"b","done":false}`+"\n"+
			`{"model":"m","response":"c","done":false}`+"\n"+
			`{"model":"m","done":true}`+"\n")
	}))
	defer engine.Close()

	rec := &recRW{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "serving-node", engine.URL, "llama"))
	p := NewProxy(NewCodec(rec), disc, 11434)

	p.handleHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"model":"llama"}`)))

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
