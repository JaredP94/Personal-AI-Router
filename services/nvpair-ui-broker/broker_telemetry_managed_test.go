// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"nvpair-shared/appdir"
)

func isolateTelemetryData(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LOCALAPPDATA", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Chdir(home)
}

func (m *mockDockerRunner) removeContainer(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = append(m.removed, name)
	m.events = append(m.events, "remove")
	return m.removeErr
}

func telemetryRPC(t *testing.T, b *Broker, method string) *Message {
	t.Helper()
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	b.codec = NewCodec(server)
	id := json.RawMessage(`1`)
	go b.handleMessage(&Message{JSONRPC: "2.0", ID: &id, Method: method})
	response, err := NewCodec(client).Read()
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func reachableTelemetryDial(string, string, time.Duration) (net.Conn, error) {
	c1, c2 := net.Pipe()
	_ = c2.Close()
	return c1, nil
}

func TestDockerManagedCommandsAndInspect(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	dir := t.TempDir()
	capture := filepath.Join(dir, "args")
	t.Setenv("DOCKER_TEST_ARGS", capture)
	docker := filepath.Join(dir, "docker")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$DOCKER_TEST_ARGS"
if [ "$1" = inspect ]; then
printf '%s\n' '[{"State":{"Status":"running"},"Config":{"Labels":{"com.docker.compose.project":"foreign","nvpair.managed-config":"canonical"}},"HostConfig":{"PortBindings":{"4317/tcp":[{"HostIp":"0.0.0.0","HostPort":"4317"}]}}}]'
fi
`
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &defaultDockerRunner{dockerPath: docker}
	details, err := runner.inspectDetails(context.Background(), "nvpair-phoenix")
	if err != nil {
		t.Fatal(err)
	}
	if details.State != "running" || details.ComposeProject != "foreign" || details.ManagedConfig != "canonical" || len(details.PortBindings["4317/tcp"]) != 1 || details.PortBindings["4317/tcp"][0].HostIP != "0.0.0.0" {
		t.Errorf("inspect lost fields: %+v", details)
	}
	for _, action := range []string{"up", "stop"} {
		if action == "up" {
			err = runner.composeUp(context.Background(), "/managed.yml")
		} else {
			err = runner.composeStop(context.Background(), "/managed.yml")
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(capture)
		want := "compose\n-p\nnvpair-telemetry\n-f\n/managed.yml\n" + action + "\n"
		if action == "up" {
			want += "-d\n"
		}
		if string(data) != want {
			t.Errorf("args = %q, want %q", data, want)
		}
	}
	if err := runner.removeContainer(context.Background(), "nvpair-phoenix"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(capture)
	if string(data) != "rm\n-f\nnvpair-phoenix\n" {
		t.Errorf("removal must not remove volumes or other containers: %q", data)
	}
}

func TestManagedPhoenixCompose(t *testing.T) {
	isolateTelemetryData(t)
	runner := &mockDockerRunner{state: "not_found"}
	b := &Broker{dockerRunner: runner, dialTimeout: reachableTelemetryDial, telemetryDesired: telemetryDesiredConfig{CollectorPort: 14317}}
	response := telemetryRPC(t, b, "telemetry/start")
	if response.Error != nil {
		t.Fatal(response.Error)
	}
	data, err := os.ReadFile(runner.composeFile)
	if err != nil {
		t.Fatal(err)
	}
	compose := string(data)
	expectedPath, err := appdir.Path("telemetry", "phoenix-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	if runner.composeFile != expectedPath {
		t.Errorf("compose file = %s, want %s", runner.composeFile, expectedPath)
	}
	info, err := os.Stat(runner.composeFile)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %o", info.Mode().Perm())
	}
	if !regexp.MustCompile(`image: arizephoenix/phoenix:version-\d+\.\d+\.\d+@sha256:[a-f0-9]{64}`).MatchString(compose) {
		t.Errorf("image is not stable and digest pinned:\n%s", compose)
	}
	for _, binding := range []string{`"127.0.0.1:6006:6006"`, `"127.0.0.1:14317:4317"`} {
		if !strings.Contains(compose, binding) {
			t.Errorf("missing binding %s", binding)
		}
	}
	bindings := regexp.MustCompile(`(?m)^\s+- "[^"\n]*:\d+:\d+"$`).FindAllString(compose, -1)
	if len(bindings) != 2 || strings.Count(strings.Join(bindings, "\n"), "127.0.0.1:") != 2 {
		t.Errorf("expected only two loopback bindings, got %v", bindings)
	}
	for _, value := range []string{"container_name: nvpair-phoenix", "name: nvpair-phoenix-data", "nvpair.managed: \"true\""} {
		if !strings.Contains(compose, value) {
			t.Errorf("missing %s", value)
		}
	}
	labelPattern := regexp.MustCompile(`nvpair.managed-config: "([a-f0-9]{64})"`)
	match := labelPattern.FindStringSubmatch(compose)
	if len(match) != 2 {
		t.Fatalf("missing managed hash:\n%s", compose)
	}
	canonical := strings.Replace(compose, match[0], `nvpair.managed-config: ""`, 1)
	if want := fmt.Sprintf("%x", sha256.Sum256([]byte(canonical))); match[1] != want {
		t.Errorf("managed hash = %s, want %s", match[1], want)
	}
	// Status must compare the canonical hash, and changes of port must invalidate it.
	runner.details = &dockerContainerDetails{
		State: "running", ManagedConfig: match[1],
		PortBindings: map[string][]dockerPortBinding{"4317/tcp": {{HostIP: "127.0.0.1", HostPort: "14317"}}},
	}
	status := b.getTelemetryStatus(context.Background())
	if string(status.ManagedConfig) != `"current"` {
		t.Errorf("managedConfig = %s", status.ManagedConfig)
	}
	if status.ImageVersion != "version-20.23.0" {
		t.Errorf("imageVersion = %s", status.ImageVersion)
	}
	runner.details.State = "stopped"
	if status = b.getTelemetryStatus(context.Background()); string(status.ManagedConfig) != `"outdated"` {
		t.Errorf("a stopped container is not the current running deployment, got %s", status.ManagedConfig)
	}
	runner.details.State = "running"
	b.telemetryDesired.CollectorPort = 24317
	if status = b.getTelemetryStatus(context.Background()); string(status.ManagedConfig) != `"outdated"` {
		t.Errorf("port change must invalidate hash, got %s", status.ManagedConfig)
	}
}

func TestStartReplacesForeignProjectContainerKeepsVolumes(t *testing.T) {
	for _, project := range []string{"another-project", "", "nvpair-telemetry"} {
		t.Run(project, func(t *testing.T) {
			isolateTelemetryData(t)
			runner := &mockDockerRunner{details: &dockerContainerDetails{State: "running", ComposeProject: project}}
			b := &Broker{dockerRunner: runner, dialTimeout: reachableTelemetryDial}
			response := telemetryRPC(t, b, "telemetry/start")
			if response.Error != nil {
				t.Fatal(response.Error)
			}
			want := "remove,up,list,sizes"
			if project == "nvpair-telemetry" {
				want = "up,list,sizes"
			}
			if strings.Join(runner.events, ",") != want {
				t.Errorf("events = %v, want %s", runner.events, want)
			}
			if project != "nvpair-telemetry" && strings.Join(runner.removed, ",") != "nvpair-phoenix" {
				t.Errorf("removed %v, want only nvpair-phoenix", runner.removed)
			}
			if runner.stopCalled {
				t.Error("must not compose-stop the foreign project")
			}
		})
	}
}

func TestStartForeignRemovalFailure(t *testing.T) {
	isolateTelemetryData(t)
	runner := &mockDockerRunner{details: &dockerContainerDetails{State: "running", ComposeProject: "foreign"}, removeErr: os.ErrPermission}
	b := &Broker{dockerRunner: runner, dialTimeout: reachableTelemetryDial}
	response := telemetryRPC(t, b, "telemetry/start")
	if response.Error == nil || response.Error.Code != -32000 {
		t.Fatalf("expected removal error, got %+v", response)
	}
	if runner.upCalled {
		t.Error("must not run compose after removal failed")
	}
}

func TestStatusFlagsExposedBindings(t *testing.T) {
	for _, tc := range []struct {
		name, bindings string
		want           bool
	}{
		{"loopback", `{"6006/tcp":[{"HostIp":"127.0.0.1","HostPort":"6006"}],"4317/tcp":[{"HostIp":"127.0.0.1","HostPort":"4317"}]}`, true},
		{"wildcard", `{"4317/tcp":[{"HostIp":"0.0.0.0","HostPort":"4317"}]}`, false},
		{"empty-ip", `{"4317/tcp":[{"HostIp":"","HostPort":"4317"}]}`, false},
		{"mixed-ipv6", `{"4317/tcp":[{"HostIp":"127.0.0.1","HostPort":"4317"},{"HostIp":"::","HostPort":"4317"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var details dockerContainerDetails
			if err := json.Unmarshal([]byte(`{"State":"running","PortBindings":`+tc.bindings+`}`), &details); err != nil {
				t.Fatal(err)
			}
			b := &Broker{dockerRunner: &mockDockerRunner{details: &details}, dialTimeout: func(string, string, time.Duration) (net.Conn, error) { return nil, net.ErrClosed }}
			status := b.getTelemetryStatus(context.Background())
			if status.BindingsLoopbackOnly == nil || *status.BindingsLoopbackOnly != tc.want {
				t.Errorf("bindingsLoopbackOnly = %v, want %t", status.BindingsLoopbackOnly, tc.want)
			}
		})
	}
}

func TestStatusManagedConfigOutdated(t *testing.T) {
	for _, state := range []string{"running", "stopped", "not_found", "docker_unavailable"} {
		t.Run(state, func(t *testing.T) {
			b := &Broker{dockerRunner: &mockDockerRunner{state: state}, dialTimeout: func(string, string, time.Duration) (net.Conn, error) { return nil, net.ErrClosed }}
			status := b.getTelemetryStatus(context.Background())
			want := `"outdated"`
			if state == "not_found" || state == "docker_unavailable" {
				want = `"missing"`
				if status.BindingsLoopbackOnly != nil {
					t.Error("no container must have null bindings")
				}
			}
			if string(status.ManagedConfig) != want {
				t.Errorf("managedConfig = %s, want %s", status.ManagedConfig, want)
			}
		})
	}
}

func TestRemovedTelemetryUpdateMethods(t *testing.T) {
	for _, method := range []string{"telemetry/check-update", "telemetry/update"} {
		t.Run(method, func(t *testing.T) {
			isolateTelemetryData(t)
			b := &Broker{dockerRunner: &mockDockerRunner{state: "not_found"}, dialTimeout: reachableTelemetryDial}
			response := telemetryRPC(t, b, method)
			if response.Error == nil || response.Error.Code != -32601 {
				t.Errorf("want -32601 for %s, got %+v", method, response)
			}
		})
	}
}
