// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type mockDockerRunner struct {
	state       string
	inspectErr  error
	upCalled    bool
	stopCalled  bool
	composeFile string
}

func (m *mockDockerRunner) inspect(ctx context.Context, containerName string) (string, error) {
	return m.state, m.inspectErr
}

func (m *mockDockerRunner) composeUp(ctx context.Context, composeFile string) error {
	m.upCalled = true
	m.composeFile = composeFile
	return nil
}

func (m *mockDockerRunner) composeStop(ctx context.Context, composeFile string) error {
	m.stopCalled = true
	m.composeFile = composeFile
	return nil
}

func TestTelemetryGetStatus(t *testing.T) {
	brokerConn, clientConn := net.Pipe()
	defer brokerConn.Close()
	defer clientConn.Close()

	settingsWorker, settingsCodec := newTestRPCWorkerPipe(t)

	go func() {
		for {
			msg, err := settingsCodec.Read()
			if err != nil {
				return
			}
			switch msg.Method {
			case "settings/get-telemetry-enabled":
				_ = settingsCodec.Respond(msg.ID, map[string]bool{"value": true})
			case "settings/get-telemetry-endpoint":
				_ = settingsCodec.Respond(msg.ID, map[string]string{"value": "127.0.0.1:4317"})
			case "settings/get-telemetry-record-payloads":
				_ = settingsCodec.Respond(msg.ID, map[string]bool{"value": false})
			default:
				_ = settingsCodec.Respond(msg.ID, map[string]any{})
			}
		}
	}()

	mockDocker := &mockDockerRunner{state: "running"}
	b := &Broker{
		codec:        NewCodec(brokerConn),
		dockerRunner: mockDocker,
		dialTimeout: func(network, address string, timeout time.Duration) (net.Conn, error) {
			c1, c2 := net.Pipe()
			_ = c2.Close()
			return c1, nil
		},
	}
	b.setSettings(settingsWorker)

	id := json.RawMessage(`1`)
	go b.handleMessage(&Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "telemetry/get-status",
	})

	resp, err := NewCodec(clientConn).Read()
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	var status TelemetryStatusResult
	if err := json.Unmarshal(resp.Result, &status); err != nil {
		t.Fatalf("failed to unmarshal status: %v", err)
	}

	if !status.Enabled {
		t.Errorf("expected Enabled=true, got false")
	}
	if status.Endpoint != "127.0.0.1:4317" {
		t.Errorf("expected Endpoint=127.0.0.1:4317, got %s", status.Endpoint)
	}
	if status.UIURL != "http://localhost:6006" {
		t.Errorf("expected UIURL=http://localhost:6006, got %s", status.UIURL)
	}
	if status.RecordPayloads {
		t.Errorf("expected RecordPayloads=false, got true")
	}
	if status.ContainerState != "running" {
		t.Errorf("expected ContainerState=running, got %s", status.ContainerState)
	}
	if !status.CollectorReachable {
		t.Errorf("expected CollectorReachable=true, got false")
	}
}

func TestTelemetryStartAndStop(t *testing.T) {
	brokerConn, clientConn := net.Pipe()
	defer brokerConn.Close()
	defer clientConn.Close()

	tempDir := t.TempDir()
	fakeCompose := filepath.Join(tempDir, "docker-compose.telemetry.yml")
	if err := os.WriteFile(fakeCompose, []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	mockDocker := &mockDockerRunner{state: "running"}
	b := &Broker{
		codec:                NewCodec(brokerConn),
		dockerRunner:         mockDocker,
		telemetryComposePath: fakeCompose,
		dialTimeout: func(network, address string, timeout time.Duration) (net.Conn, error) {
			c1, c2 := net.Pipe()
			_ = c2.Close()
			return c1, nil
		},
	}

	// Test telemetry/start
	id1 := json.RawMessage(`1`)
	go b.handleMessage(&Message{
		JSONRPC: "2.0",
		ID:      &id1,
		Method:  "telemetry/start",
	})

	resp1, err := NewCodec(clientConn).Read()
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}
	if resp1.Error != nil {
		t.Fatalf("unexpected error on telemetry/start: %v", resp1.Error)
	}
	if !mockDocker.upCalled {
		t.Errorf("expected composeUp to be called")
	}

	// Test telemetry/stop
	mockDocker.state = "stopped"
	id2 := json.RawMessage(`2`)
	go b.handleMessage(&Message{
		JSONRPC: "2.0",
		ID:      &id2,
		Method:  "telemetry/stop",
	})

	resp2, err := NewCodec(clientConn).Read()
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}
	if resp2.Error != nil {
		t.Fatalf("unexpected error on telemetry/stop: %v", resp2.Error)
	}
	if !mockDocker.stopCalled {
		t.Errorf("expected composeStop to be called")
	}
}

func TestDeriveUIURL(t *testing.T) {
	cases := []struct {
		endpoint string
		expected string
	}{
		{"localhost:4317", "http://localhost:6006"},
		{"127.0.0.1:4317", "http://localhost:6006"},
		{"192.168.1.50:4317", "http://192.168.1.50:6006"},
		{"[::1]:4317", "http://localhost:6006"},
		{"2001:db8::1:4317", "http://[2001:db8::1]:6006"},
		{"[2001:db8::1]:4317", "http://[2001:db8::1]:6006"},
		{"http://192.168.1.50:4317", "http://192.168.1.50:6006"},
		{"", "http://localhost:6006"},
	}

	for _, tc := range cases {
		got := deriveUIURL(tc.endpoint)
		if got != tc.expected {
			t.Errorf("deriveUIURL(%q) = %q, want %q", tc.endpoint, got, tc.expected)
		}
	}
}

func TestTelemetryGetStatusDockerUnavailable(t *testing.T) {
	brokerConn, clientConn := net.Pipe()
	defer brokerConn.Close()
	defer clientConn.Close()

	mockDocker := &mockDockerRunner{state: "docker_unavailable"}
	b := &Broker{
		codec:        NewCodec(brokerConn),
		dockerRunner: mockDocker,
		dialTimeout: func(network, address string, timeout time.Duration) (net.Conn, error) {
			return nil, net.ErrClosed
		},
	}

	id := json.RawMessage(`10`)
	go b.handleMessage(&Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "telemetry/get-status",
	})

	resp, err := NewCodec(clientConn).Read()
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	var status TelemetryStatusResult
	if err := json.Unmarshal(resp.Result, &status); err != nil {
		t.Fatalf("failed to unmarshal status: %v", err)
	}

	if status.ContainerState != "docker_unavailable" {
		t.Errorf("expected ContainerState=docker_unavailable, got %s", status.ContainerState)
	}
	if status.CollectorReachable {
		t.Errorf("expected CollectorReachable=false, got true")
	}
	if !status.Enabled {
		t.Errorf("expected default Enabled=true, got false")
	}
}

type errorDockerRunner struct {
	upErr   error
	stopErr error
}

func (e *errorDockerRunner) inspect(ctx context.Context, containerName string) (string, error) {
	return "stopped", nil
}

func (e *errorDockerRunner) composeUp(ctx context.Context, composeFile string) error {
	return e.upErr
}

func (e *errorDockerRunner) composeStop(ctx context.Context, composeFile string) error {
	return e.stopErr
}

func TestTelemetryStartAndStopErrors(t *testing.T) {
	brokerConn, clientConn := net.Pipe()
	defer brokerConn.Close()
	defer clientConn.Close()

	tempDir := t.TempDir()
	fakeCompose := filepath.Join(tempDir, "docker-compose.telemetry.yml")
	if err := os.WriteFile(fakeCompose, []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	errDocker := &errorDockerRunner{
		upErr:   os.ErrPermission,
		stopErr: os.ErrPermission,
	}
	b := &Broker{
		codec:                NewCodec(brokerConn),
		dockerRunner:         errDocker,
		telemetryComposePath: fakeCompose,
	}

	// Test composeUp error
	id1 := json.RawMessage(`1`)
	go b.handleMessage(&Message{
		JSONRPC: "2.0",
		ID:      &id1,
		Method:  "telemetry/start",
	})
	resp1, err := NewCodec(clientConn).Read()
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}
	if resp1.Error == nil || resp1.Error.Code != -32000 {
		t.Fatalf("expected -32000 error, got: %+v", resp1.Error)
	}

	// Test composeStop error
	id2 := json.RawMessage(`2`)
	go b.handleMessage(&Message{
		JSONRPC: "2.0",
		ID:      &id2,
		Method:  "telemetry/stop",
	})
	resp2, err := NewCodec(clientConn).Read()
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}
	if resp2.Error == nil || resp2.Error.Code != -32000 {
		t.Fatalf("expected -32000 error, got: %+v", resp2.Error)
	}
}

func TestResolveComposeFilePath(t *testing.T) {
	tempDir := t.TempDir()
	composePath := filepath.Join(tempDir, "docker-compose.telemetry.yml")
	if err := os.WriteFile(composePath, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. Explicit path
	resolved, err := resolveComposeFilePath(composePath)
	if err != nil {
		t.Fatalf("expected success with explicit path: %v", err)
	}
	if resolved != composePath {
		t.Errorf("resolved = %s, want %s", resolved, composePath)
	}

	// 2. Env variable
	t.Setenv("NVPAIR_TELEMETRY_COMPOSE_PATH", composePath)
	resolvedEnv, err := resolveComposeFilePath("")
	if err != nil {
		t.Fatalf("expected success with env var: %v", err)
	}
	if resolvedEnv != composePath {
		t.Errorf("resolvedEnv = %s, want %s", resolvedEnv, composePath)
	}

	// 3. Non-existent path returns error when nothing matches
	t.Setenv("NVPAIR_TELEMETRY_COMPOSE_PATH", filepath.Join(tempDir, "nonexistent.yml"))
	_, err = resolveComposeFilePath(filepath.Join(tempDir, "also-nonexistent.yml"))
	if err == nil {
		t.Fatalf("expected error for non-existent path")
	}
}

