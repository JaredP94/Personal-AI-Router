// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

type mockDockerRunner struct {
	mu          sync.Mutex
	state       string
	inspectErr  error
	upCalled    bool
	stopCalled  bool
	composeFile string
	details     *dockerContainerDetails
	removed     []string
	removeErr   error
	events      []string
}

func (m *mockDockerRunner) setState(state string) {
	m.mu.Lock()
	m.state = state
	m.mu.Unlock()
}

func (m *mockDockerRunner) composeUpCalled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.upCalled
}

func (m *mockDockerRunner) composeStopCalled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopCalled
}

func (m *mockDockerRunner) inspectDetails(ctx context.Context, containerName string) (dockerContainerDetails, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.details != nil {
		return *m.details, m.inspectErr
	}
	return dockerContainerDetails{
		State: m.state,
	}, m.inspectErr
}

func (m *mockDockerRunner) composeUp(ctx context.Context, composeFile string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.upCalled = true
	m.events = append(m.events, "up")
	m.composeFile = composeFile
	return nil
}

func (m *mockDockerRunner) composeStop(ctx context.Context, composeFile string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
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
			case "settings/get-telemetry-config":
				_ = settingsCodec.Respond(msg.ID, telemetryDesiredConfig{Revision: 1, Enabled: true, CollectorPort: 4317, PayloadPolicyVersion: 1})
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
	b.loadTelemetryConfig(settingsWorker)

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
	if status.CollectorPort != 4317 {
		t.Errorf("expected CollectorPort=4317, got %d", status.CollectorPort)
	}
	if status.UIURL != "http://127.0.0.1:6006" {
		t.Errorf("expected UIURL=http://127.0.0.1:6006, got %s", status.UIURL)
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

	isolateTelemetryData(t)

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
	if !mockDocker.composeUpCalled() {
		t.Errorf("expected composeUp to be called")
	}

	// Test telemetry/stop
	mockDocker.setState("stopped")
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
	if !mockDocker.composeStopCalled() {
		t.Errorf("expected composeStop to be called")
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
	if status.Enabled || status.Revision != 0 {
		t.Errorf("expected missing settings to be disabled at revision 0, got %+v", status)
	}
}

type errorDockerRunner struct {
	upErr   error
	stopErr error
}

func (e *errorDockerRunner) inspectDetails(ctx context.Context, containerName string) (dockerContainerDetails, error) {
	return dockerContainerDetails{State: "stopped"}, nil
}

func (e *errorDockerRunner) composeUp(ctx context.Context, composeFile string) error {
	return e.upErr
}

func (e *errorDockerRunner) composeStop(ctx context.Context, composeFile string) error {
	return e.stopErr
}

func (e *errorDockerRunner) removeContainer(context.Context, string) error { return nil }

func TestTelemetryStartAndStopErrors(t *testing.T) {
	brokerConn, clientConn := net.Pipe()
	defer brokerConn.Close()
	defer clientConn.Close()

	isolateTelemetryData(t)

	errDocker := &errorDockerRunner{
		upErr:   os.ErrPermission,
		stopErr: os.ErrPermission,
	}
	b := &Broker{
		codec:        NewCodec(brokerConn),
		dockerRunner: errDocker,
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

type slowDockerRunner struct {
	mu             sync.Mutex
	composeUpDelay time.Duration
	state          string
}

func (s *slowDockerRunner) stateSnapshot() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *slowDockerRunner) inspectDetails(ctx context.Context, containerName string) (dockerContainerDetails, error) {
	return dockerContainerDetails{State: s.stateSnapshot()}, nil
}

func (s *slowDockerRunner) composeUp(ctx context.Context, composeFile string) error {
	select {
	case <-time.After(s.composeUpDelay):
		s.mu.Lock()
		s.state = "running"
		s.mu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *slowDockerRunner) composeStop(ctx context.Context, composeFile string) error {
	s.mu.Lock()
	s.state = "stopped"
	s.mu.Unlock()
	return nil
}

func (s *slowDockerRunner) removeContainer(context.Context, string) error { return nil }

func TestTelemetryStart_SlowPullBackgrounding(t *testing.T) {
	brokerConn, clientConn := net.Pipe()
	defer brokerConn.Close()
	defer clientConn.Close()

	isolateTelemetryData(t)

	// composeUp takes 2.2 seconds, which exceeds the 1.5s quick wait
	slowRunner := &slowDockerRunner{
		composeUpDelay: 2200 * time.Millisecond,
		state:          "not_found",
	}

	b := &Broker{
		codec:        NewCodec(brokerConn),
		dockerRunner: slowRunner,
		dialTimeout: func(network, address string, timeout time.Duration) (net.Conn, error) {
			if slowRunner.stateSnapshot() == "running" {
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
