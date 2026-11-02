// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nvpair-shared/clustertrust"
)

// TestStreamOpEmitsProgressThenResult verifies streamOp forwards published
// progress frames and ends with the run's terminal result frame.
func TestStreamOpEmitsProgressThenResult(t *testing.T) {
	exec := &Executor{progress: newProgressHub()}
	s := &controlServer{exec: exec}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", controlInstallPath, nil)

	st := EngineStatus{Engine: "ollama", Installed: true, Running: true}
	s.streamOp(rec, req, "op1", "ollama", "install", func(ctx context.Context) (streamFrame, error) {
		exec.progress.publish(ProgressEvent{Engine: "ollama", Op: "install", Stage: "downloading", Percent: 42})
		return streamFrame{Type: "result", OpID: "op1", Engine: "ollama", Op: "install", Status: &st}, nil
	})

	frames := decodeFrames(t, rec.Body.String())
	if len(frames) != 2 {
		t.Fatalf("expected 2 frames (progress, result), got %d: %s", len(frames), rec.Body.String())
	}
	if frames[0].Type != "progress" || frames[0].Percent != 42 || frames[0].OpID != "op1" {
		t.Fatalf("bad progress frame: %+v", frames[0])
	}
	if frames[1].Type != "result" || frames[1].Status == nil || !frames[1].Status.Running {
		t.Fatalf("bad result frame: %+v", frames[1])
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("expected ndjson content-type, got %q", ct)
	}
}

// TestStreamOpEmitsErrorFrame verifies a failing op yields a terminal error
// frame rather than a result.
func TestStreamOpEmitsErrorFrame(t *testing.T) {
	exec := &Executor{progress: newProgressHub()}
	s := &controlServer{exec: exec}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", controlInstallPath, nil)

	s.streamOp(rec, req, "op2", "ollama", "install", func(ctx context.Context) (streamFrame, error) {
		return streamFrame{}, context.DeadlineExceeded
	})

	frames := decodeFrames(t, rec.Body.String())
	if len(frames) != 1 || frames[0].Type != "error" || frames[0].Message == "" {
		t.Fatalf("expected one error frame, got %+v", frames)
	}
}

// TestHandleInstallRejectsMissingEngine verifies request-body validation.
func TestHandleInstallRejectsMissingEngine(t *testing.T) {
	s := &controlServer{exec: &Executor{progress: newProgressHub()}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", controlInstallPath, strings.NewReader(`{"opId":"x"}`))
	s.handleInstall(rec, req)
	if rec.Code != 400 {
		t.Fatalf("expected 400 for missing engine, got %d", rec.Code)
	}
}

// newToolControlPeer reuses the model gate's identities and exercises the real
// ec mux over mTLS. The initiator pins the server even when it is not pinned back.
func newToolControlPeer(t *testing.T, ex *Executor, pinned bool) (*httptest.Server, *clustertrust.Mesh) {
	t.Helper()
	serverCert, serverKey := mintLeaf(t, "uuid-server")
	callerCert, callerKey := mintLeaf(t, "uuid-caller")
	pins := map[string][]byte{}
	if pinned {
		pins["uuid-caller"] = callerCert
	}
	serverMesh := clustertrust.Open(clusterDirFor(t, serverCert, serverKey, pins))
	callerMesh := clustertrust.Open(clusterDirFor(t, callerCert, callerKey, map[string][]byte{"uuid-server": serverCert}))
	srv := httptest.NewUnstartedServer((&controlServer{exec: ex, mesh: serverMesh}).mux())
	srv.TLS = serverMesh.ServerTLSConfig()
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, callerMesh
}

func toolControlRequest(t *testing.T, srv *httptest.Server, mesh *clustertrust.Mesh, method, body string) (int, http.Header, string) {
	t.Helper()
	cfg, ok := mesh.ClientTLSConfig("uuid-server")
	if !ok {
		t.Fatal("caller must pin server")
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
	defer client.CloseIdleConnections()
	req, err := http.NewRequest(method, srv.URL+"/v1/tools/install", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(data)
}

func TestToolInstallRouteRefusesUnpinnedCaller(t *testing.T) {
	ex, _, _, _ := newToolTestExecutor(t)
	srv, mesh := newToolControlPeer(t, ex, false)
	code, _, body := toolControlRequest(t, srv, mesh, "POST", `{"tool":"macmon"}`)
	if code != 403 {
		t.Fatalf("unpinned caller: HTTP %d, want 403: %s", code, body)
	}
}

func TestToolInstallRouteStreamsCodedError(t *testing.T) {
	ex, _, _, _ := newToolTestExecutor(t)
	srv, mesh := newToolControlPeer(t, ex, true)
	code, headers, body := toolControlRequest(t, srv, mesh, "POST", `{"opId":"tool-op","tool":"macmon"}`)
	if code != 200 || headers.Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("expected NDJSON HTTP 200, got %d: %s", code, body)
	}
	var frame struct {
		Type    string `json:"type"`
		OpID    string `json:"opId"`
		Op      string `json:"op"`
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(body), &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Type != "error" || frame.OpID != "tool-op" || frame.Op != "install-tool" || frame.Code != -32010 || frame.Message != "Homebrew was not found" {
		t.Fatalf("bad coded error frame: %s", body)
	}
}

func TestToolInstallRouteResult(t *testing.T) {
	ex, brew, detect, _ := newToolTestExecutor(t)
	writeToolFile(t, brew, fmt.Sprintf("#!/bin/sh\nprintf '#!/bin/sh\\n' > %q\n/bin/chmod +x %q\n", detect, detect), 0o755)
	srv, mesh := newToolControlPeer(t, ex, true)
	code, _, body := toolControlRequest(t, srv, mesh, "POST", `{"opId":"tool-op","tool":"macmon"}`)
	if code != 200 {
		t.Fatalf("HTTP %d: %s", code, body)
	}
	frames := decodeFrames(t, body)
	if len(frames) != 1 || frames[0].Type != "result" || frames[0].Op != "install-tool" || frames[0].OpID != "tool-op" {
		t.Fatalf("bad terminal result: %s", body)
	}
	var result struct {
		Tool      string `json:"tool"`
		Installed bool   `json:"installed"`
	}
	if err := json.Unmarshal(frames[0].Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.Tool != "macmon" || !result.Installed {
		t.Fatalf("unexpected tool result: %s", frames[0].Result)
	}
}

func TestToolInstallRouteValidation(t *testing.T) {
	for _, tc := range []struct {
		name, method, body string
		status             int
	}{
		{"method", "GET", "", 405},
		{"invalid JSON", "POST", "{", 400},
		{"empty tool", "POST", `{"tool":""}`, 400},
		{"missing tool", "POST", `{}`, 400},
		{"unknown tool", "POST", `{"tool":"rm"}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex, _, _, _ := newToolTestExecutor(t)
			srv, mesh := newToolControlPeer(t, ex, true)
			code, headers, body := toolControlRequest(t, srv, mesh, tc.method, tc.body)
			if code != tc.status {
				t.Fatalf("HTTP %d, want %d: %s", code, tc.status, body)
			}
			if tc.status == 405 && headers.Get("Allow") != "POST" {
				t.Fatalf("Allow = %q, want POST", headers.Get("Allow"))
			}
			if tc.name == "unknown tool" {
				frames := decodeFrames(t, body)
				if len(frames) != 1 || frames[0].Type != "error" || frames[0].Message != `unknown tool "rm"` || strings.Contains(body, `"code"`) {
					t.Fatalf("unknown tool must yield a codeless error: %s", body)
				}
			}
		})
	}
}

func decodeFrames(t *testing.T, body string) []streamFrame {
	t.Helper()
	var out []streamFrame
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var f streamFrame
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatalf("bad frame line %q: %v", line, err)
		}
		out = append(out, f)
	}
	return out
}
