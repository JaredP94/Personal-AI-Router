// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// TelemetryStatusResult reports the runtime state of the local Arize Phoenix collector
// and current telemetry configuration.
type TelemetryStatusResult struct {
	Enabled            bool   `json:"enabled"`
	Endpoint           string `json:"endpoint"`
	UIURL              string `json:"uiUrl"`
	RecordPayloads     bool   `json:"recordPayloads"`
	ContainerState     string `json:"containerState"`
	CollectorReachable bool   `json:"collectorReachable"`
}

var dialTimeout = net.DialTimeout

type dockerRunner interface {
	inspect(ctx context.Context, containerName string) (string, error)
	composeUp(ctx context.Context, composeFile string) error
	composeStop(ctx context.Context, composeFile string) error
}

type defaultDockerRunner struct {
	dockerPath string
}

func (r *defaultDockerRunner) getDockerPath() string {
	if r.dockerPath != "" {
		return r.dockerPath
	}
	if p, err := exec.LookPath("docker"); err == nil {
		return p
	}
	for _, candidate := range []string{"/usr/local/bin/docker", "/opt/homebrew/bin/docker", "/usr/bin/docker"} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "docker"
}

func (r *defaultDockerRunner) inspect(ctx context.Context, containerName string) (string, error) {
	dockerBin := r.getDockerPath()
	cmd := exec.CommandContext(ctx, dockerBin, "inspect", "--format={{.State.Status}}", containerName)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	outStr := strings.TrimSpace(stdout.String())
	errStr := strings.ToLower(stderr.String())

	if err != nil {
		if strings.Contains(errStr, "no such object") || strings.Contains(errStr, "no such container") {
			return "not_found", nil
		}
		if strings.Contains(errStr, "cannot connect to the docker daemon") ||
			strings.Contains(errStr, "error during connect") ||
			strings.Contains(errStr, "is the docker daemon running") ||
			errors.Is(err, exec.ErrNotFound) {
			return "docker_unavailable", nil
		}
		return "docker_unavailable", fmt.Errorf("docker inspect failed: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}

	switch outStr {
	case "running":
		return "running", nil
	case "exited":
		return "stopped", nil
	case "":
		return "not_found", nil
	default:
		return outStr, nil
	}
}

func (r *defaultDockerRunner) composeUp(ctx context.Context, composeFile string) error {
	dockerBin := r.getDockerPath()
	cmd := exec.CommandContext(ctx, dockerBin, "compose", "-f", composeFile, "up", "-d")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("compose up failed: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (r *defaultDockerRunner) composeStop(ctx context.Context, composeFile string) error {
	dockerBin := r.getDockerPath()
	cmd := exec.CommandContext(ctx, dockerBin, "compose", "-f", composeFile, "stop")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("compose stop failed: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (b *Broker) getDockerRunner() dockerRunner {
	if b.dockerRunner != nil {
		return b.dockerRunner
	}
	return &defaultDockerRunner{}
}

func resolveComposeFilePath(customPath string) (string, error) {
	if customPath != "" {
		if _, err := os.Stat(customPath); err == nil {
			return filepath.Abs(customPath)
		}
		return "", fmt.Errorf("custom compose file not found: %s", customPath)
	}

	if envPath := os.Getenv("NVPAIR_TELEMETRY_COMPOSE_PATH"); envPath != "" {
		if _, err := os.Stat(envPath); err == nil {
			return filepath.Abs(envPath)
		}
		return "", fmt.Errorf("compose file from NVPAIR_TELEMETRY_COMPOSE_PATH not found: %s", envPath)
	}

	candidates := []string{
		"docker-compose.telemetry.yml",
		"../docker-compose.telemetry.yml",
		"../../docker-compose.telemetry.yml",
	}

	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(exeDir, "docker-compose.telemetry.yml"),
			filepath.Join(exeDir, "..", "docker-compose.telemetry.yml"),
			filepath.Join(exeDir, "..", "..", "docker-compose.telemetry.yml"),
		)
	}

	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return filepath.Abs(c)
		}
	}

	return "", fmt.Errorf("docker-compose.telemetry.yml not found")
}

func deriveUIURL(endpoint string) string {
	if endpoint == "" {
		return "http://localhost:6006"
	}
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		host = endpoint
	}
	if host == "" || host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "[::1]" {
		return "http://localhost:6006"
	}
	return fmt.Sprintf("http://%s:6006", host)
}

func probeCollectorReachable(endpoint string, timeout time.Duration) bool {
	if endpoint == "" {
		return false
	}
	conn, err := dialTimeout("tcp", endpoint, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func getBoolSettingValue(ctx context.Context, settings *rpcWorker, method string) *bool {
	result, rpcErr, err := settings.Call(ctx, method, nil)
	if err != nil || rpcErr != nil {
		return nil
	}
	var r struct {
		Value bool `json:"value"`
	}
	if err := json.Unmarshal(result, &r); err != nil {
		return nil
	}
	return &r.Value
}

func (b *Broker) getTelemetryEndpointSetting() string {
	settings := b.getSettings()
	if settings == nil {
		return "localhost:4317"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if ep := getSettingValue(ctx, settings, "settings/get-telemetry-endpoint"); ep != "" {
		return ep
	}
	return "localhost:4317"
}

func (b *Broker) getTelemetryStatus(ctx context.Context) TelemetryStatusResult {
	settings := b.getSettings()

	enabled := true
	endpoint := "localhost:4317"
	recordPayloads := false

	if settings != nil {
		sCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()

		if v := getBoolSettingValue(sCtx, settings, "settings/get-telemetry-enabled"); v != nil {
			enabled = *v
		}
		if ep := getSettingValue(sCtx, settings, "settings/get-telemetry-endpoint"); ep != "" {
			endpoint = ep
		}
		if v := getBoolSettingValue(sCtx, settings, "settings/get-telemetry-record-payloads"); v != nil {
			recordPayloads = *v
		}
	}

	containerState := "docker_unavailable"
	runner := b.getDockerRunner()
	if state, err := runner.inspect(ctx, "nvpair-phoenix"); err == nil {
		containerState = state
	}

	reachable := probeCollectorReachable(endpoint, 500*time.Millisecond)

	return TelemetryStatusResult{
		Enabled:            enabled,
		Endpoint:           endpoint,
		UIURL:              deriveUIURL(endpoint),
		RecordPayloads:     recordPayloads,
		ContainerState:     containerState,
		CollectorReachable: reachable,
	}
}

func (b *Broker) handleTelemetryGetStatus(msg *Message) {
	status := b.getTelemetryStatus(context.Background())
	if err := b.codec.Respond(msg.ID, status); err != nil {
		log.Printf("failed to respond to telemetry/get-status: %v", err)
	}
}

func (b *Broker) handleTelemetryStart(msg *Message) {
	composePath, err := resolveComposeFilePath(b.telemetryComposePath)
	if err != nil {
		if err := b.codec.RespondError(msg.ID, -32000, fmt.Sprintf("cannot resolve compose file: %v", err)); err != nil {
			log.Printf("failed to respond to telemetry/start error: %v", err)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	runner := b.getDockerRunner()
	if err := runner.composeUp(ctx, composePath); err != nil {
		if err := b.codec.RespondError(msg.ID, -32000, fmt.Sprintf("docker compose up failed: %v", err)); err != nil {
			log.Printf("failed to respond to telemetry/start error: %v", err)
		}
		return
	}

	endpoint := b.getTelemetryEndpointSetting()
	pollDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(pollDeadline) {
		if probeCollectorReachable(endpoint, 250*time.Millisecond) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	slog.Info("telemetry collector container started", "composePath", composePath, "endpoint", endpoint)
	status := b.getTelemetryStatus(context.Background())
	if err := b.codec.Respond(msg.ID, status); err != nil {
		log.Printf("failed to respond to telemetry/start: %v", err)
	}
}

func (b *Broker) handleTelemetryStop(msg *Message) {
	composePath, err := resolveComposeFilePath(b.telemetryComposePath)
	if err != nil {
		if err := b.codec.RespondError(msg.ID, -32000, fmt.Sprintf("cannot resolve compose file: %v", err)); err != nil {
			log.Printf("failed to respond to telemetry/stop error: %v", err)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	runner := b.getDockerRunner()
	if err := runner.composeStop(ctx, composePath); err != nil {
		if err := b.codec.RespondError(msg.ID, -32000, fmt.Sprintf("docker compose stop failed: %v", err)); err != nil {
			log.Printf("failed to respond to telemetry/stop error: %v", err)
		}
		return
	}

	slog.Info("telemetry collector container stopped", "composePath", composePath)
	status := b.getTelemetryStatus(context.Background())
	if err := b.codec.Respond(msg.ID, status); err != nil {
		log.Printf("failed to respond to telemetry/stop: %v", err)
	}
}
