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
	"io"
	"log"
	"log/slog"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"nvpair-shared/appdir"
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
	Storage              telemetryStorage       `json:"storage"`
}

const traceVolumeLabel = "com.docker.compose.volume=nvpair-phoenix-data"

type telemetryVolumeStorage struct {
	Name  string `json:"name"`
	Bytes *int64 `json:"bytes"`
}

type telemetryStorage struct {
	Bytes      *int64                   `json:"bytes"`
	Volumes    []telemetryVolumeStorage `json:"volumes"`
	MeasuredAt *string                  `json:"measuredAt"`
	Error      *string                  `json:"error"`
}

type telemetryPurgeResult struct {
	Removed []string                 `json:"removed"`
	Refused []telemetryVolumeRefusal `json:"refused"`
	Storage telemetryStorage         `json:"storage"`
}

type telemetryVolumeRefusal struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

type dockerVolume struct {
	Name string `json:"Name"`
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
	removeStoppedContainer(ctx context.Context, containerName string) error
	composeUp(ctx context.Context, composeFile string) error
	composeStop(ctx context.Context, composeFile string) error
	listVolumes(ctx context.Context, labelFilter string) ([]dockerVolume, error)
	volumeSizes(ctx context.Context) (map[string]int64, error)
	volumeUsers(ctx context.Context, name string) ([]string, error)
	removeVolume(ctx context.Context, name string) error
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

// Force removal is only for foreign-project replacement. No -v: trace volumes survive.
func (r *defaultDockerRunner) removeContainer(ctx context.Context, containerName string) error {
	cmd := exec.CommandContext(ctx, r.getDockerPath(), "rm", "-f", containerName)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("remove container failed: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Non-force removal lets Docker reject a container restarted since the users snapshot.
func (r *defaultDockerRunner) removeStoppedContainer(ctx context.Context, containerName string) error {
	cmd := exec.CommandContext(ctx, r.getDockerPath(), "rm", containerName)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("remove stopped container failed: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
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

// storageCommand never invokes a shell. Volume removal deliberately has no
// force/prune flags; Docker remains the final guard against attached volumes.
func (r *defaultDockerRunner) storageCommand(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.getDockerPath(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker %s failed: %w (stderr: %s)", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (r *defaultDockerRunner) listVolumes(ctx context.Context, labelFilter string) ([]dockerVolume, error) {
	output, err := r.storageCommand(ctx, "volume", "ls", "--filter", "label="+labelFilter, "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	volumes := make([]dockerVolume, 0)
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var volume dockerVolume
		if err := decoder.Decode(&volume); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decode docker volumes: %w", err)
		}
		if volume.Name == "" || strings.HasPrefix(volume.Name, "-") {
			return nil, errors.New("docker returned an invalid volume name")
		}
		volumes = append(volumes, volume)
	}
	return volumes, nil
}

// Docker's verbose formatter returns a Volumes array with human-readable SI
// sizes (e.g. 1.024kB), not the engine API's UsageData.Size integers. Report the
// bytes represented by that measurement; do not interpret unavailable sizes as 0.
func (r *defaultDockerRunner) volumeSizes(ctx context.Context) (map[string]int64, error) {
	output, err := r.storageCommand(ctx, "system", "df", "-v", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	var usage struct {
		Volumes *[]struct {
			Name string
			Size string
		}
	}
	if err := json.Unmarshal(output, &usage); err != nil {
		return nil, fmt.Errorf("decode docker disk usage: %w", err)
	}
	if usage.Volumes == nil {
		return nil, errors.New("docker disk usage has no volumes measurement")
	}
	sizes := make(map[string]int64)
	for _, volume := range *usage.Volumes {
		var multiplier float64
		var number string
		for i, unit := range []string{"B", "kB", "MB", "GB", "TB", "PB", "EB"} {
			if strings.HasSuffix(volume.Size, unit) {
				multiplier = math.Pow(1000, float64(i))
				number = strings.TrimSpace(strings.TrimSuffix(volume.Size, unit))
			}
		}
		value, err := strconv.ParseFloat(number, 64)
		value = math.Round(value * multiplier)
		if err != nil || multiplier == 0 || math.IsNaN(value) || value < 0 || value >= float64(math.MaxInt64) || volume.Name == "" {
			return nil, fmt.Errorf("docker volume %q has an unavailable or invalid size", volume.Name)
		}
		sizes[volume.Name] = int64(value)
	}
	return sizes, nil
}

func (r *defaultDockerRunner) volumeUsers(ctx context.Context, name string) ([]string, error) {
	output, err := r.storageCommand(ctx, "ps", "-a", "--filter", "volume="+name, "--format", "{{.Names}}|{{.State}}")
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(string(output))
	if text == "" {
		return []string{}, nil
	}
	return strings.Split(text, "\n"), nil
}

func (r *defaultDockerRunner) removeVolume(ctx context.Context, name string) error {
	_, err := r.storageCommand(ctx, "volume", "rm", name)
	return err
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
		Storage:                b.measureTelemetryStorage(ctx),
	}
}

func (b *Broker) measureTelemetryStorage(parent context.Context) telemetryStorage {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	storage := telemetryStorage{Volumes: make([]telemetryVolumeStorage, 0)}
	failed := func(err error) telemetryStorage {
		message := err.Error()
		storage.Error = &message
		return storage
	}
	runner := b.getDockerRunner()
	volumes, err := runner.listVolumes(ctx, traceVolumeLabel)
	if err != nil {
		return failed(err)
	}
	sort.Slice(volumes, func(i, j int) bool { return volumes[i].Name < volumes[j].Name })
	for _, volume := range volumes {
		storage.Volumes = append(storage.Volumes, telemetryVolumeStorage{Name: volume.Name})
	}
	sizes, err := runner.volumeSizes(ctx)
	if err != nil {
		return failed(err)
	}
	var total int64
	for i := range storage.Volumes {
		volume := &storage.Volumes[i]
		size, found := sizes[volume.Name]
		if !found || size < 0 || total > math.MaxInt64-size {
			return failed(fmt.Errorf("size unavailable for trace volume %q", volume.Name))
		}
		volume.Bytes = &size
		total += size
	}
	if err := ctx.Err(); err != nil {
		return failed(err)
	}
	measuredAt := time.Now().UTC().Format(time.RFC3339Nano)
	storage.Bytes = &total
	storage.MeasuredAt = &measuredAt
	return storage
}

func purgeTraceVolume(ctx context.Context, runner dockerRunner, name string) error {
	users, err := runner.volumeUsers(ctx, name)
	if err != nil {
		return err
	}
	removePhoenix := false
	// Check every user before removing even a stopped Phoenix container.
	for _, user := range users {
		container, state, ok := strings.Cut(strings.TrimSpace(user), "|")
		if !ok || container == "" {
			return errors.New("cannot determine volume users")
		}
		switch state {
		case "exited", "dead", "created":
			if container == "nvpair-phoenix" {
				removePhoenix = true
			}
		case "running", "paused", "restarting":
			return fmt.Errorf("volume is used by active container %q", container)
		default:
			return fmt.Errorf("cannot safely remove volume used by container %q in state %q", container, state)
		}
	}
	if removePhoenix {
		if err := runner.removeStoppedContainer(ctx, "nvpair-phoenix"); err != nil {
			return fmt.Errorf("container %q is in use or cannot be removed: %w", "nvpair-phoenix", err)
		}
	}
	return runner.removeVolume(ctx, name)
}

func (b *Broker) purgeTelemetryStorage(ctx context.Context) (telemetryPurgeResult, error) {
	b.telemetryMu.Lock()
	defer b.telemetryMu.Unlock()
	result := telemetryPurgeResult{Removed: make([]string, 0), Refused: make([]telemetryVolumeRefusal, 0)}
	if b.telemetryCancelStart != nil {
		b.telemetryCancelStart()
		b.telemetryCancelStart = nil
	}
	b.telemetryStarting.Store(false)
	composePath, err := b.writePhoenixCompose()
	if err != nil {
		return result, fmt.Errorf("cannot write managed compose file: %w", err)
	}
	runner := b.getDockerRunner()
	stopCtx, stopCancel := context.WithTimeout(ctx, 20*time.Second)
	err = runner.composeStop(stopCtx, composePath)
	stopCancel()
	if err != nil {
		return result, fmt.Errorf("cannot stop managed Phoenix: %w", err)
	}
	listCtx, listCancel := context.WithTimeout(ctx, 10*time.Second)
	volumes, err := runner.listVolumes(listCtx, traceVolumeLabel)
	listCancel()
	if err != nil {
		return result, fmt.Errorf("cannot list trace volumes: %w", err)
	}
	sort.Slice(volumes, func(i, j int) bool { return volumes[i].Name < volumes[j].Name })
	for _, volume := range volumes {
		volumeCtx, volumeCancel := context.WithTimeout(ctx, 20*time.Second)
		err := purgeTraceVolume(volumeCtx, runner, volume.Name)
		volumeCancel()
		if err != nil {
			result.Refused = append(result.Refused, telemetryVolumeRefusal{Name: volume.Name, Reason: err.Error()})
		} else {
			result.Removed = append(result.Removed, volume.Name)
		}
	}
	result.Storage = b.measureTelemetryStorage(ctx)
	return result, nil
}

func (b *Broker) handleTelemetryPurgeStorage(msg *Message) {
	var params struct {
		Confirm string `json:"confirm"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil || params.Confirm != "delete-traces" {
		if err := b.codec.RespondError(msg.ID, -32602, "confirm must be delete-traces"); err != nil {
			log.Printf("failed to respond to telemetry/purge-storage: %v", err)
		}
		return
	}
	result, err := b.purgeTelemetryStorage(context.Background())
	if err != nil {
		if err := b.codec.RespondError(msg.ID, -32000, err.Error()); err != nil {
			log.Printf("failed to respond to telemetry/purge-storage: %v", err)
		}
		return
	}
	if err := b.codec.Respond(msg.ID, result); err != nil {
		log.Printf("failed to respond to telemetry/purge-storage: %v", err)
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

	slog.Info("telemetry collector container stopped", "composePath", composePath)
	status := b.getTelemetryStatus(context.Background())
	if err := b.codec.Respond(msg.ID, status); err != nil {
		log.Printf("failed to respond to telemetry/stop: %v", err)
	}
}
