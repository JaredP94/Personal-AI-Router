// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type mockDockerRunner struct {
	state          string
	inspectErr     error
	upCalled       bool
	stopCalled     bool
	pullCalled     bool
	composeFile    string
	version        string
	imageID        string
	latestVer      string
	latestImageID  string
	latestDigest   string
}

func (m *mockDockerRunner) inspect(ctx context.Context, containerName string) (string, error) {
	return m.state, m.inspectErr
}

func (m *mockDockerRunner) inspectDetails(ctx context.Context, containerName string) (dockerContainerDetails, error) {
	return dockerContainerDetails{
		State:   m.state,
		ImageID: m.imageID,
		Version: m.version,
	}, m.inspectErr
}

func (m *mockDockerRunner) inspectImage(ctx context.Context, imageName string) (dockerImageDetails, error) {
	return dockerImageDetails{
		ImageID: m.latestImageID,
		Version: m.latestVer,
		Digest:  m.latestDigest,
	}, nil
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

func (m *mockDockerRunner) composePull(ctx context.Context, composeFile string) error {
	m.pullCalled = true
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
	pullErr error
}

func (e *errorDockerRunner) inspect(ctx context.Context, containerName string) (string, error) {
	return "stopped", nil
}

func (e *errorDockerRunner) inspectDetails(ctx context.Context, containerName string) (dockerContainerDetails, error) {
	return dockerContainerDetails{State: "stopped"}, nil
}

func (e *errorDockerRunner) inspectImage(ctx context.Context, imageName string) (dockerImageDetails, error) {
	return dockerImageDetails{}, nil
}

func (e *errorDockerRunner) composeUp(ctx context.Context, composeFile string) error {
	return e.upErr
}

func (e *errorDockerRunner) composeStop(ctx context.Context, composeFile string) error {
	return e.stopErr
}

func (e *errorDockerRunner) composePull(ctx context.Context, composeFile string) error {
	return e.pullErr
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

func TestResolveComposeFilePath_FallbackProvisioning(t *testing.T) {
	tempHome := t.TempDir()
	t.Chdir(tempHome)
	t.Setenv("HOME", tempHome)
	t.Setenv("LOCALAPPDATA", tempHome)
	t.Setenv("XDG_CONFIG_HOME", tempHome)
	t.Setenv("NVPAIR_TELEMETRY_COMPOSE_PATH", "")

	// When customPath is empty, and candidates next to exe/cwd do not exist,
	// resolveComposeFilePath should provision and return the default compose file in appdir.
	resolved, err := resolveComposeFilePath("")
	if err != nil {
		t.Fatalf("expected fallback provisioning to succeed, got: %v", err)
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		t.Fatalf("failed to read provisioned compose file: %v", err)
	}
	if !strings.Contains(string(data), "arizephoenix/phoenix:latest") {
		t.Errorf("expected provisioned compose file to contain arizephoenix/phoenix:latest, got: %s", string(data))
	}
}

type slowDockerRunner struct {
	composeUpDelay time.Duration
	state          string
}

func (s *slowDockerRunner) inspect(ctx context.Context, containerName string) (string, error) {
	return s.state, nil
}

func (s *slowDockerRunner) inspectDetails(ctx context.Context, containerName string) (dockerContainerDetails, error) {
	return dockerContainerDetails{State: s.state}, nil
}

func (s *slowDockerRunner) inspectImage(ctx context.Context, imageName string) (dockerImageDetails, error) {
	return dockerImageDetails{}, nil
}

func (s *slowDockerRunner) composeUp(ctx context.Context, composeFile string) error {
	select {
	case <-time.After(s.composeUpDelay):
		s.state = "running"
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *slowDockerRunner) composeStop(ctx context.Context, composeFile string) error {
	s.state = "stopped"
	return nil
}

func (s *slowDockerRunner) composePull(ctx context.Context, composeFile string) error {
	return nil
}

func TestTelemetryStart_SlowPullBackgrounding(t *testing.T) {
	brokerConn, clientConn := net.Pipe()
	defer brokerConn.Close()
	defer clientConn.Close()

	tempDir := t.TempDir()
	fakeCompose := filepath.Join(tempDir, "docker-compose.telemetry.yml")
	if err := os.WriteFile(fakeCompose, []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// composeUp takes 2.2 seconds, which exceeds the 1.5s quick wait
	slowRunner := &slowDockerRunner{
		composeUpDelay: 2200 * time.Millisecond,
		state:          "not_found",
	}

	b := &Broker{
		codec:                NewCodec(brokerConn),
		dockerRunner:         slowRunner,
		telemetryComposePath: fakeCompose,
		dialTimeout: func(network, address string, timeout time.Duration) (net.Conn, error) {
			if slowRunner.state == "running" {
				c1, c2 := net.Pipe()
				_ = c2.Close()
				return c1, nil
			}
			return nil, net.ErrClosed
		},
	}

	start := time.Now()
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
	elapsed := time.Since(start)

	if resp1.Error != nil {
		t.Fatalf("unexpected error on telemetry/start: %v", resp1.Error)
	}

	// Should respond in ~1.5s rather than blocking for 2.2s+
	if elapsed > 2000*time.Millisecond {
		t.Errorf("expected response within 2s, took %v", elapsed)
	}

	var status TelemetryStatusResult
	if err := json.Unmarshal(resp1.Result, &status); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if status.ContainerState != "starting" {
		t.Errorf("expected ContainerState='starting', got %q", status.ContainerState)
	}

	// Subsequent get-status during pull should also report 'starting'
	pollStatus := b.getTelemetryStatus(context.Background())
	if pollStatus.ContainerState != "starting" {
		t.Errorf("expected polled ContainerState='starting' while pull ongoing, got %q", pollStatus.ContainerState)
	}

	// Wait for background composeUp to finish
	time.Sleep(1000 * time.Millisecond)

	finalStatus := b.getTelemetryStatus(context.Background())
	if finalStatus.ContainerState != "running" {
		t.Errorf("expected final ContainerState='running', got %q", finalStatus.ContainerState)
	}
}

func TestTelemetryCheckUpdate(t *testing.T) {
	brokerConn, clientConn := net.Pipe()
	defer brokerConn.Close()
	defer clientConn.Close()

	mockDocker := &mockDockerRunner{
		state:         "running",
		imageID:       "sha256:oldimage123",
		version:       "arize-phoenix-v20.16.0",
		latestImageID: "sha256:newimage456",
		latestVer:     "arize-phoenix-v20.19.0",
	}

	b := &Broker{
		codec:        NewCodec(brokerConn),
		dockerRunner: mockDocker,
		telemetryHTTPClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				body := `{"name":"latest","digest":"sha256:newimage456","tag_last_pushed":"2026-10-01T23:05:00Z"}`
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(body)),
					Header:     make(http.Header),
				}, nil
			}),
		},
		dialTimeout: func(network, address string, timeout time.Duration) (net.Conn, error) {
			c1, c2 := net.Pipe()
			_ = c2.Close()
			return c1, nil
		},
	}

	id := json.RawMessage(`1`)
	go b.handleMessage(&Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "telemetry/check-update",
	})

	resp, err := NewCodec(clientConn).Read()
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error on telemetry/check-update: %v", resp.Error)
	}

	var status TelemetryStatusResult
	if err := json.Unmarshal(resp.Result, &status); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if !status.UpdateAvailable {
		t.Errorf("expected UpdateAvailable=true, got false")
	}
	if status.CurrentVersion != "v20.16.0" {
		t.Errorf("expected CurrentVersion=v20.16.0, got %q", status.CurrentVersion)
	}
	if status.LatestVersion != "v20.19.0" {
		t.Errorf("expected LatestVersion=v20.19.0, got %q", status.LatestVersion)
	}
}

func TestTelemetryUpdate(t *testing.T) {
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

	id := json.RawMessage(`2`)
	go b.handleMessage(&Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "telemetry/update",
	})

	resp, err := NewCodec(clientConn).Read()
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error on telemetry/update: %v", resp.Error)
	}

	if !mockDocker.pullCalled {
		t.Errorf("expected composePull to be called")
	}
	if !mockDocker.upCalled {
		t.Errorf("expected composeUp to be called")
	}
}

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}




