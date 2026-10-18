// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
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
	"text/template"
	"time"

	"nvpair-shared/appdir"
	"nvpair-shared/noderec"
)

const phoenixImageVersion = "version-20.23.0"

//go:embed telemetry/phoenix-compose.yml.tmpl
var phoenixComposeTemplate string

var parsedPhoenixCompose = template.Must(template.New("phoenix-compose").Parse(phoenixComposeTemplate))

// renderPhoenixCompose fingerprints the canonical render with the managed-config
// label value empty, then renders again with that SHA-256 inserted. Hashing the
// final file would be self-referential. Status uses this same canonical hash.
func renderPhoenixCompose(collectorPort int) ([]byte, string, error) {
	data := struct {
		CollectorPort int
		ImageVersion  string
		ManagedConfig string
	}{CollectorPort: collectorPort, ImageVersion: phoenixImageVersion}
	var canonical bytes.Buffer
	if err := parsedPhoenixCompose.Execute(&canonical, data); err != nil {
		return nil, "", err
	}
	data.ManagedConfig = fmt.Sprintf("%x", sha256.Sum256(canonical.Bytes()))
	var rendered bytes.Buffer
	if err := parsedPhoenixCompose.Execute(&rendered, data); err != nil {
		return nil, "", err
	}
	return rendered.Bytes(), data.ManagedConfig, nil
}

func (b *Broker) writePhoenixCompose() (string, error) {
	b.telemetryConfigMu.Lock()
	desired := b.desiredTelemetryLocked()
	b.telemetryConfigMu.Unlock()
	rendered, _, err := renderPhoenixCompose(desired.CollectorPort)
	if err != nil {
		return "", err
	}
	composePath, err := appdir.Path("telemetry", "phoenix-compose.yml")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(composePath), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(composePath, rendered, 0o644); err != nil {
		return "", err
	}
	if err := os.Chmod(composePath, 0o644); err != nil {
		return "", err
	}
	return composePath, nil
}

// TelemetryStatusResult reports the runtime state of the local Arize Phoenix collector
// and current telemetry configuration.
type TelemetryStatusResult struct {
	telemetryDesiredConfig
	Workers              []workerTelemetryState `json:"workers"`
	ContainerState       string                 `json:"containerState"`
	CollectorReachable   bool                   `json:"collectorReachable"`
	UIURL                string                 `json:"uiUrl"`
	ImageVersion         string                 `json:"imageVersion"`
	BindingsLoopbackOnly *bool                  `json:"bindingsLoopbackOnly"`
	ManagedConfig        json.RawMessage        `json:"managedConfig"`
	Storage              json.RawMessage        `json:"storage"`
}

type dockerPortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type dockerContainerDetails struct {
	State          string
	ComposeProject string
	ManagedConfig  string
	PortBindings   map[string][]dockerPortBinding
}

type dockerRunner interface {
	inspectDetails(ctx context.Context, containerName string) (dockerContainerDetails, error)
	removeContainer(ctx context.Context, containerName string) error
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

func (r *defaultDockerRunner) inspectDetails(ctx context.Context, containerName string) (dockerContainerDetails, error) {
	dockerBin := r.getDockerPath()
	cmd := exec.CommandContext(ctx, dockerBin, "inspect", containerName)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	errStr := strings.ToLower(stderr.String())

	if err != nil {
		if strings.Contains(errStr, "no such object") || strings.Contains(errStr, "no such container") {
			return dockerContainerDetails{State: "not_found"}, nil
		}
		if strings.Contains(errStr, "cannot connect to the docker daemon") ||
			strings.Contains(errStr, "error during connect") ||
			strings.Contains(errStr, "is the docker daemon running") ||
			errors.Is(err, exec.ErrNotFound) {
			return dockerContainerDetails{State: "docker_unavailable"}, nil
		}
		return dockerContainerDetails{State: "docker_unavailable"}, fmt.Errorf("docker inspect failed: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}

	var containers []struct {
		State      struct{ Status string }
		Config     struct{ Labels map[string]string }
		HostConfig struct {
			PortBindings map[string][]dockerPortBinding
		}
	}
	if err := json.Unmarshal(stdout.Bytes(), &containers); err != nil {
		return dockerContainerDetails{}, fmt.Errorf("decode docker inspect: %w", err)
	}
	if len(containers) != 1 {
		return dockerContainerDetails{}, fmt.Errorf("docker inspect returned %d containers", len(containers))
	}
	container := containers[0]
	state := container.State.Status
	if state == "exited" {
		state = "stopped"
	}
	return dockerContainerDetails{
		State:          state,
		ComposeProject: container.Config.Labels["com.docker.compose.project"],
		ManagedConfig:  container.Config.Labels["nvpair.managed-config"],
		PortBindings:   container.HostConfig.PortBindings,
	}, nil
}

// Force removal stops only this named container. No -v: trace volumes survive.
func (r *defaultDockerRunner) removeContainer(ctx context.Context, containerName string) error {
	cmd := exec.CommandContext(ctx, r.getDockerPath(), "rm", "-f", containerName)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("remove container failed: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (r *defaultDockerRunner) getComposeArgs(composeFile string, actionArgs ...string) []string {
	args := []string{"compose", "-p", "nvpair-telemetry", "-f", composeFile}
	return append(args, actionArgs...)
}

func (r *defaultDockerRunner) composeUp(ctx context.Context, composeFile string) error {
	dockerBin := r.getDockerPath()
	args := r.getComposeArgs(composeFile, "up", "-d")
	cmd := exec.CommandContext(ctx, dockerBin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("compose up failed: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (r *defaultDockerRunner) composeStop(ctx context.Context, composeFile string) error {
	dockerBin := r.getDockerPath()
	args := r.getComposeArgs(composeFile, "stop")
	cmd := exec.CommandContext(ctx, dockerBin, args...)
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

func (b *Broker) getTelemetryEndpointSetting() string {
	b.telemetryConfigMu.Lock()
	desired := b.desiredTelemetryLocked()
	b.telemetryConfigMu.Unlock()
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(desired.CollectorPort))
}

func (b *Broker) getTelemetryStatus(ctx context.Context) TelemetryStatusResult {
	desired, workers, proxies := b.telemetrySnapshot()
	b.queryTelemetryHealth(ctx, workers, proxies)

	containerState := "docker_unavailable"
	var bindingsLoopbackOnly *bool
	managedConfig := json.RawMessage(`"missing"`)
	_, expectedHash, renderErr := renderPhoenixCompose(desired.CollectorPort)
	runner := b.getDockerRunner()
	iCtx, iCancel := context.WithTimeout(ctx, 3*time.Second)
	defer iCancel()
	if details, err := runner.inspectDetails(iCtx, "nvpair-phoenix"); err == nil {
		containerState = details.State
		if details.State != "not_found" && details.State != "docker_unavailable" && details.State != "" {
			loopbackOnly := true
			for _, bindings := range details.PortBindings {
				for _, binding := range bindings {
					if binding.HostIP != "127.0.0.1" {
						loopbackOnly = false
					}
				}
			}
			bindingsLoopbackOnly = &loopbackOnly
			managedConfig = json.RawMessage(`"outdated"`)
			if details.State == "running" && renderErr == nil && details.ManagedConfig == expectedHash {
				managedConfig = json.RawMessage(`"current"`)
			}
		}
	}
	if b.telemetryStarting.Load() && containerState != "running" {
		containerState = "starting"
	}
	endpoint := net.JoinHostPort("127.0.0.1", strconv.Itoa(desired.CollectorPort))
	return TelemetryStatusResult{
		telemetryDesiredConfig: desired,
		Workers:                workers,
		ContainerState:         containerState,
		CollectorReachable:     b.probeCollector(endpoint, 500*time.Millisecond),
		UIURL:                  "http://127.0.0.1:6006",
		ImageVersion:           phoenixImageVersion,
		BindingsLoopbackOnly:   bindingsLoopbackOnly,
		ManagedConfig:          managedConfig,
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

	composePath, err := b.writePhoenixCompose()
	if err != nil {
		b.telemetryMu.Unlock()
		if err := b.codec.RespondError(msg.ID, -32000, fmt.Sprintf("cannot write managed compose file: %v", err)); err != nil {
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
		details, err := runner.inspectDetails(startCtx, "nvpair-phoenix")
		if err != nil {
			upDone <- err
			return
		}
		if details.State != "not_found" && details.State != "docker_unavailable" && details.State != "" && details.ComposeProject != "nvpair-telemetry" {
			if err := runner.removeContainer(startCtx, "nvpair-phoenix"); err != nil {
				upDone <- err
				return
			}
		}
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

	composePath, err := b.writePhoenixCompose()
	if err != nil {
		if err := b.codec.RespondError(msg.ID, -32000, fmt.Sprintf("cannot write managed compose file: %v", err)); err != nil {
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
