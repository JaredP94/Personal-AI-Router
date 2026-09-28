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
	"strconv"
	"strings"
	"time"

	"nvpair-shared/appdir"
	"nvpair-shared/noderec"
)

const defaultTelemetryComposeYAML = `# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

services:
  phoenix:
    image: arizephoenix/phoenix:latest
    container_name: nvpair-phoenix
    restart: unless-stopped
    ports:
      - "6006:6006" # Phoenix UI and OTLP HTTP
      - "4317:4317" # OTLP gRPC collector
    environment:
      - PHOENIX_PORT=6006
      - PHOENIX_GRPC_PORT=4317
      - PHOENIX_SQL_DATABASE_URL=sqlite:////data/phoenix.db
    volumes:
      - nvpair-phoenix-data:/data

volumes:
  nvpair-phoenix-data:
    driver: local
`

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
		"../../../docker-compose.telemetry.yml",
	}

	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(exeDir, "docker-compose.telemetry.yml"),
			filepath.Join(exeDir, "..", "docker-compose.telemetry.yml"),
			filepath.Join(exeDir, "..", "..", "docker-compose.telemetry.yml"),
			filepath.Join(exeDir, "..", "..", "..", "docker-compose.telemetry.yml"),
		)
	}

	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return filepath.Abs(c)
		}
	}

	if appDirPath, err := appdir.Path("docker-compose.telemetry.yml"); err == nil {
		if _, err := os.Stat(appDirPath); err == nil {
			return filepath.Abs(appDirPath)
		}
		if err := os.MkdirAll(filepath.Dir(appDirPath), 0o700); err == nil {
			if err := os.WriteFile(appDirPath, []byte(defaultTelemetryComposeYAML), 0o644); err == nil {
				slog.Info("provisioned default telemetry compose file into appdir", "path", appDirPath)
				return filepath.Abs(appDirPath)
			}
		}
	}

	return "", fmt.Errorf("docker-compose.telemetry.yml not found")
}

func deriveUIURL(endpoint string) string {
	if endpoint == "" {
		return "http://localhost:6006"
	}
	clean := endpoint
	clean = strings.TrimPrefix(clean, "http://")
	clean = strings.TrimPrefix(clean, "https://")

	host, _, err := net.SplitHostPort(clean)
	if err != nil {
		if lastColon := strings.LastIndex(clean, ":"); lastColon != -1 && strings.Count(clean, ":") > 1 {
			candidateHost := clean[:lastColon]
			candidatePort := clean[lastColon+1:]
			if _, pErr := strconv.Atoi(candidatePort); pErr == nil {
				host = candidateHost
			} else {
				host = clean
			}
		} else {
			host = clean
		}
	}
	if host == "" || host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "[::1]" {
		return "http://localhost:6006"
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("http://%s:6006", host)
}

func (b *Broker) probeCollector(endpoint string, timeout time.Duration) bool {
	if endpoint == "" {
		return false
	}
	dial := net.DialTimeout
	if b.dialTimeout != nil {
		dial = b.dialTimeout
	}
	conn, err := dial("tcp", endpoint, timeout)
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
	iCtx, iCancel := context.WithTimeout(ctx, 3*time.Second)
	defer iCancel()
	if state, err := runner.inspect(iCtx, "nvpair-phoenix"); err == nil {
		containerState = state
	}

	if b.telemetryStarting.Load() && containerState != "running" {
		containerState = "starting"
	}

	reachable := b.probeCollector(endpoint, 500*time.Millisecond)

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
	b.telemetryMu.Lock()

	// If already in starting state, return the in-progress status immediately
	if b.telemetryStarting.Load() {
		b.telemetryMu.Unlock()
		status := b.getTelemetryStatus(context.Background())
		if err := b.codec.Respond(msg.ID, status); err != nil {
			log.Printf("failed to respond to telemetry/start: %v", err)
		}
		return
	}

	composePath, err := resolveComposeFilePath(b.telemetryComposePath)
	if err != nil {
		b.telemetryMu.Unlock()
		if err := b.codec.RespondError(msg.ID, -32000, fmt.Sprintf("cannot resolve compose file: %v", err)); err != nil {
			log.Printf("failed to respond to telemetry/start error: %v", err)
		}
		return
	}

	// Cancel any previously active startup context
	if b.telemetryCancelStart != nil {
		b.telemetryCancelStart()
	}
	startCtx, startCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	b.telemetryCancelStart = startCancel
	b.telemetryStarting.Store(true)

	runner := b.getDockerRunner()
	b.telemetryMu.Unlock()

	upDone := make(chan error, 1)
	go func() {
		upDone <- runner.composeUp(startCtx, composePath)
	}()

	// Wait up to 1500ms for quick local/cached start or instant test mocks
	select {
	case err := <-upDone:
		b.telemetryStarting.Store(false)

		if err != nil {
			if err := b.codec.RespondError(msg.ID, -32000, fmt.Sprintf("docker compose up failed: %v", err)); err != nil {
				log.Printf("failed to respond to telemetry/start error: %v", err)
			}
			return
		}

		endpoint := b.getTelemetryEndpointSetting()
		pollDeadline := time.Now().Add(2 * time.Second)
		var reachable bool
		for time.Now().Before(pollDeadline) {
			if b.probeCollector(endpoint, 250*time.Millisecond) {
				reachable = true
				break
			}
			time.Sleep(150 * time.Millisecond)
		}

		if reachable {
			slog.Info("telemetry collector container started and accepting connections", "composePath", composePath, "endpoint", endpoint)
			b.registerService(noderec.RegisterParams{Service: noderec.ServiceOTel, Port: 4317})
		}

		finalStatus := b.getTelemetryStatus(context.Background())
		if err := b.codec.Respond(msg.ID, finalStatus); err != nil {
			log.Printf("failed to respond to telemetry/start: %v", err)
		}
		return

	case <-time.After(1500 * time.Millisecond):
		// Asynchronous / slow path: docker is pulling the image or initializing slowly.
		// Hand off to background worker so the JSON-RPC call returns "starting" immediately.
		slog.Info("docker compose up taking time (likely pulling container image); backgrounding startup monitor", "composePath", composePath)

		go func() {
			err := <-upDone
			b.telemetryStarting.Store(false)

			if err != nil {
				if startCtx.Err() == nil {
					slog.Error("background docker compose up failed", "err", err)
				}
				return
			}

			slog.Info("docker compose up completed, polling collector reachability", "composePath", composePath)
			endpoint := b.getTelemetryEndpointSetting()
			pollDeadline := time.Now().Add(45 * time.Second)
			var reachable bool
			for time.Now().Before(pollDeadline) && startCtx.Err() == nil {
				if b.probeCollector(endpoint, 500*time.Millisecond) {
					reachable = true
					break
				}
				time.Sleep(500 * time.Millisecond)
			}

			if reachable {
				slog.Info("telemetry collector container started and accepting connections", "composePath", composePath, "endpoint", endpoint)
				b.registerService(noderec.RegisterParams{Service: noderec.ServiceOTel, Port: 4317})
			} else {
				slog.Warn("telemetry collector container started but collector port not responding within deadline", "composePath", composePath, "endpoint", endpoint)
			}
		}()

		startingStatus := b.getTelemetryStatus(context.Background())
		startingStatus.ContainerState = "starting"
		if err := b.codec.Respond(msg.ID, startingStatus); err != nil {
			log.Printf("failed to respond to telemetry/start: %v", err)
		}
	}
}

func (b *Broker) handleTelemetryStop(msg *Message) {
	b.telemetryMu.Lock()
	if b.telemetryCancelStart != nil {
		b.telemetryCancelStart()
		b.telemetryCancelStart = nil
	}
	b.telemetryStarting.Store(false)
	b.telemetryMu.Unlock()

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

	b.unregisterService(noderec.ServiceOTel)
	slog.Info("telemetry collector container stopped", "composePath", composePath)
	status := b.getTelemetryStatus(context.Background())
	if err := b.codec.Respond(msg.ID, status); err != nil {
		log.Printf("failed to respond to telemetry/stop: %v", err)
	}
}

