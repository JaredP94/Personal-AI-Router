// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	// The engine is part of the job's identity: every proxy counts its own
	// requests from 1, so an ollama job "1" and another engine's job "1" on
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

// TestForwardedPeerRequestCarriesTheJobIdentity: a request forwarded to a
// cluster peer carries the origin's job id, the origin's node id and the
// origin's run nonce. The serving peer needs all three to publish the job's
// live frame under the origin's identity. A request that is not forwarded to
// a peer carries none of them — the peer is the only node that needs them.
func TestForwardedPeerRequestCarriesTheJobIdentity(t *testing.T) {
	p := testProxy(NewDiscovery(), 11435)
	p.nodeID = "node-origin"

	peer := candidate{
		id:       "peer-a",
		url:      &url.URL{Scheme: "https", Host: "192.0.2.10:1234"},
		peerUUID: "principal-peer",
	}
	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	p.forwardDirector(peer, "42")(req)
	if got := req.Header.Get(nvpairRequestIDHeader); got != "42" {
		t.Fatalf("%s = %q, want the job id 42", nvpairRequestIDHeader, got)
	}
	if got := req.Header.Get(nvpairOriginNodeHeader); got != "node-origin" {
		t.Fatalf("%s = %q, want the origin's node id", nvpairOriginNodeHeader, got)
	}
	if got := req.Header.Get(nvpairRunIDHeader); got != p.runID {
		t.Fatalf("%s = %q, want the proxy's run nonce %q", nvpairRunIDHeader, got, p.runID)
	}

	local := candidate{
		id:  "local-node",
		url: &url.URL{Scheme: "http", Host: "127.0.0.1:8000"},
	}
	req = httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	p.forwardDirector(local, "42")(req)
	if got := req.Header.Get(nvpairRequestIDHeader); got != "" {
		t.Fatalf("%s = %q, want it absent on a request that is not forwarded to a peer", nvpairRequestIDHeader, got)
	}
	if got := req.Header.Get(nvpairOriginNodeHeader); got != "" {
		t.Fatalf("%s = %q, want it absent on a request that is not forwarded to a peer", nvpairOriginNodeHeader, got)
	}
	if got := req.Header.Get(nvpairRunIDHeader); got != "" {
		t.Fatalf("%s = %q, want it absent on a request that is not forwarded to a peer", nvpairRunIDHeader, got)
	}
}

// TestTheServingSideAttributesTheFrameToTheOrigin: a request a peer
// forwarded is measured on the serving side too, and the frame the serving
// side publishes is attributed to the ORIGIN of the job. A frame published
// without the origin would be stamped with the serving node's own id by the
// serving node's broker, and the job's terminal event — which names the
// origin — would never retire that view.
func TestTheServingSideAttributesTheFrameToTheOrigin(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"m","response":"a","done":false}`+"\n"+
			`{"model":"m","response":"b","done":false}`+"\n"+
			`{"model":"m","done":true}`+"\n")
	}))
	defer engine.Close()

	rec := &recRW{}
	p := NewProxy(NewCodec(rec), NewDiscovery(), 11435)
	p.nodeID = "node-peer"

	target, err := url.Parse(engine.URL)
	if err != nil {
		t.Fatal(err)
	}
	rp := p.newLocalReverseProxy(target, peerJob{
		jobID:        "job-99",
		originNodeID: "origin-a",
		runID:        "run-1",
		start:        time.Now(),
	})
	rp.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"model":"m"}`)))

	frames := progressNotifications(t, rec)
	if len(frames) == 0 {
		t.Fatal("the serving side published no live frame for the peer-served job")
	}
	frame := frames[0]
	if frame.JobID != "job-99" {
		t.Fatalf("the frame's job id = %q, want the origin's job id 99", frame.JobID)
	}
	if frame.OriginatedFrom != "origin-a" {
		t.Fatalf("the frame's origin = %q, want the origin that forwarded the request", frame.OriginatedFrom)
	}
	if frame.RunID != "run-1" {
		t.Fatalf("the frame's run id = %q, want the origin's run nonce", frame.RunID)
	}
	if frame.NodeID != "node-peer" {
		t.Fatalf("the frame names node %q, want the serving node", frame.NodeID)
	}
	if frame.Engine != workloadEngine {
		t.Fatalf("the frame names engine %q, want %q", frame.Engine, workloadEngine)
	}
	if frame.Bytes == nil || *frame.Bytes <= 0 {
		t.Fatalf("the frame reports bytes %v, want the bytes the engine sent", frame.Bytes)
	}
	if frame.ElapsedMs == nil {
		t.Fatal("the frame reports no elapsed time")
	}
	// The serving side relays the engine's stream without parsing it, so it
	// counts no content chunks: a frame must never claim a chunk count it
	// did not measure.
	if frame.Chunks != nil {
		t.Fatalf("the frame reports chunks %v, want none — the serving side does not parse the engine's stream", *frame.Chunks)
	}
}
