// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

const testTraceLabel = "com.docker.compose.volume=nvpair-phoenix-data"

func (m *mockDockerRunner) listVolumes(ctx context.Context, labelFilter string) ([]dockerVolume, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, "list")
	if deadline, ok := ctx.Deadline(); ok {
		m.storageTimeout = time.Until(deadline)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	volumes := make([]dockerVolume, 0)
	for name, label := range m.volumeLabels {
		if label == labelFilter {
			volumes = append(volumes, dockerVolume{Name: name})
		}
	}
	sort.Slice(volumes, func(i, j int) bool { return volumes[i].Name < volumes[j].Name })
	return volumes, m.listErr
}

func (m *mockDockerRunner) volumeSizes(ctx context.Context) (map[string]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, "sizes")
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return m.volumeBytes, m.sizesErr
}

func (m *mockDockerRunner) volumeUsers(ctx context.Context, name string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, "users:"+name)
	return m.users[name], m.usersErr
}

func (m *mockDockerRunner) removeVolume(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, "volume-rm:"+name)
	m.removedVolumes = append(m.removedVolumes, name)
	if m.volumeRemoveErr != nil {
		return m.volumeRemoveErr
	}
	delete(m.volumeLabels, name)
	return nil
}

type storageWire struct {
	Bytes   *int64 `json:"bytes"`
	Volumes []struct {
		Name  string `json:"name"`
		Bytes *int64 `json:"bytes"`
	} `json:"volumes"`
	MeasuredAt *string `json:"measuredAt"`
	Error      *string `json:"error"`
}

type purgeWire struct {
	Removed []string `json:"removed"`
	Refused []struct {
		Name   string `json:"name"`
		Reason string `json:"reason"`
	} `json:"refused"`
	Storage storageWire `json:"storage"`
}

func storageTestBroker(runner *mockDockerRunner) *Broker {
	return &Broker{dockerRunner: runner, dialTimeout: func(string, string, time.Duration) (net.Conn, error) { return nil, net.ErrClosed }}
}

func storageRPC(t *testing.T, b *Broker, method, params string) *Message {
	t.Helper()
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	b.codec = NewCodec(server)
	id := json.RawMessage(`1`)
	go b.handleMessage(&Message{JSONRPC: "2.0", ID: &id, Method: method, Params: json.RawMessage(params)})
	response, err := NewCodec(client).Read()
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func decodeStorageStatus(t *testing.T, response *Message) storageWire {
	t.Helper()
	if response.Error != nil {
		t.Fatal(response.Error)
	}
	var status struct {
		Storage *storageWire `json:"storage"`
	}
	if err := json.Unmarshal(response.Result, &status); err != nil {
		t.Fatal(err)
	}
	if status.Storage == nil {
		t.Fatal("storage must be an object, got null")
	}
	return *status.Storage
}

// A missing label filter would count the unrelated database; omitting legacy
// Compose projects would lose the earlier PAIR volume.
func TestTelemetryStorageOnlyTraceVolumes(t *testing.T) {
	runner := &mockDockerRunner{
		volumeLabels: map[string]string{"nvpair-phoenix-data": testTraceLabel, "old-project_nvpair-phoenix-data": testTraceLabel, "database": "nvpair.managed=true"},
		volumeBytes:  map[string]int64{"nvpair-phoenix-data": 1024, "old-project_nvpair-phoenix-data": 2048, "database": 999999},
	}
	storage := decodeStorageStatus(t, storageRPC(t, storageTestBroker(runner), "telemetry/get-status", `{}`))
	if storage.Bytes == nil || *storage.Bytes != 3072 {
		t.Fatalf("trace bytes = %v, want 3072", storage.Bytes)
	}
	if len(storage.Volumes) != 2 || storage.Volumes[0].Name != "nvpair-phoenix-data" || storage.Volumes[1].Name != "old-project_nvpair-phoenix-data" {
		t.Fatalf("volumes = %+v", storage.Volumes)
	}
	if storage.Volumes[0].Bytes == nil || *storage.Volumes[0].Bytes != 1024 || storage.Volumes[1].Bytes == nil || *storage.Volumes[1].Bytes != 2048 {
		t.Errorf("per-volume bytes = %+v", storage.Volumes)
	}
	if storage.Error != nil || storage.MeasuredAt == nil {
		t.Fatalf("measurement metadata = %+v", storage)
	}
	if _, err := time.Parse(time.RFC3339Nano, *storage.MeasuredAt); err != nil {
		t.Fatal(err)
	}
	if runner.storageTimeout <= 0 || runner.storageTimeout > 10*time.Second {
		t.Errorf("storage timeout = %v, want <= 10s", runner.storageTimeout)
	}
}

func TestTelemetryStorageFailuresAreNotZero(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runner *mockDockerRunner
	}{
		{"docker-unavailable", &mockDockerRunner{listErr: errors.New("Docker unavailable")}},
		{"size-command", &mockDockerRunner{volumeLabels: map[string]string{"trace": testTraceLabel}, sizesErr: errors.New("measurement failed")}},
		{"missing-size", &mockDockerRunner{volumeLabels: map[string]string{"trace": testTraceLabel}}},
		{"negative-size", &mockDockerRunner{volumeLabels: map[string]string{"trace": testTraceLabel}, volumeBytes: map[string]int64{"trace": -1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storage := decodeStorageStatus(t, storageRPC(t, storageTestBroker(tc.runner), "telemetry/get-status", `{}`))
			if storage.Bytes != nil || storage.Error == nil || *storage.Error == "" || storage.MeasuredAt != nil {
				t.Errorf("failed measurement = %+v", storage)
			}
			if storage.Volumes == nil {
				t.Error("volumes must be an array")
			}
		})
	}
}

func TestTelemetryStorageEmptyIsZero(t *testing.T) {
	storage := decodeStorageStatus(t, storageRPC(t, storageTestBroker(&mockDockerRunner{}), "telemetry/get-status", `{}`))
	if storage.Bytes == nil || *storage.Bytes != 0 || storage.Volumes == nil || len(storage.Volumes) != 0 || storage.Error != nil {
		t.Errorf("empty storage = %+v", storage)
	}
}

func TestTelemetryPurgeRequiresConfirmation(t *testing.T) {
	for _, params := range []string{"", `{}`, `null`, `{"confirm":"yes"}`, `{"confirm":true}`, `{"confirm":"delete-traces"`} {
		t.Run(params, func(t *testing.T) {
			runner := &mockDockerRunner{}
			response := storageRPC(t, storageTestBroker(runner), "telemetry/purge-storage", params)
			if response.Error == nil || response.Error.Code != -32602 {
				t.Fatalf("want -32602, got %+v", response)
			}
			if runner.stopCalled || len(runner.events) != 0 {
				t.Errorf("invalid confirmation caused Docker operations: %v", runner.events)
			}
		})
	}
}

func TestTelemetryPurgeScopedToTraceVolumes(t *testing.T) {
	isolateTelemetryData(t)
	runner := &mockDockerRunner{
		volumeLabels: map[string]string{"nvpair-phoenix-data": testTraceLabel, "old-project_nvpair-phoenix-data": testTraceLabel, "database": "nvpair.managed=true"},
		volumeBytes:  map[string]int64{"nvpair-phoenix-data": 1024, "old-project_nvpair-phoenix-data": 2048, "database": 999999},
		users:        map[string][]string{"nvpair-phoenix-data": {"nvpair-phoenix|exited"}},
	}
	response := storageRPC(t, storageTestBroker(runner), "telemetry/purge-storage", `{"confirm":"delete-traces"}`)
	if response.Error != nil {
		t.Fatal(response.Error)
	}
	var result purgeWire
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatal(err)
	}
	want := []string{"nvpair-phoenix-data", "old-project_nvpair-phoenix-data"}
	if !reflect.DeepEqual(result.Removed, want) || result.Refused == nil || len(result.Refused) != 0 {
		t.Fatalf("purge result = %+v", result)
	}
	if !reflect.DeepEqual(runner.removedVolumes, want) || !reflect.DeepEqual(runner.removed, []string{"nvpair-phoenix"}) {
		t.Errorf("removed volumes %v; containers %v", runner.removedVolumes, runner.removed)
	}
	if runner.volumeLabels["database"] == "" {
		t.Error("unrelated volume deleted")
	}
	if result.Storage.Bytes == nil || *result.Storage.Bytes != 0 || result.Storage.Error != nil {
		t.Errorf("post-purge storage = %+v", result.Storage)
	}
	if runner.stopTimeout <= 0 || runner.stopTimeout > 20*time.Second {
		t.Errorf("stop timeout = %v, want <= 20s", runner.stopTimeout)
	}
	events := strings.Join(runner.events, ",")
	if !strings.HasPrefix(events, "stop,list,users:nvpair-phoenix-data,remove,volume-rm:nvpair-phoenix-data,users:old-project_nvpair-phoenix-data,volume-rm:old-project_nvpair-phoenix-data,") || strings.Contains(events, "prune") {
		t.Errorf("unsafe operation order: %s", events)
	}
}

func TestTelemetryPurgeRefusesUnsafeVolumes(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		users                          []string
		usersErr, removeErr, volumeErr error
	}{
		{name: "running-non-pair", users: []string{"nvpair-phoenix|exited", "database|running"}},
		{name: "running-pair", users: []string{"nvpair-phoenix|running"}},
		{name: "paused", users: []string{"database|paused"}},
		{name: "malformed-users", users: []string{"unparseable"}},
		{name: "users-failure", usersErr: errors.New("cannot list users")},
		{name: "container-removal-failure", users: []string{"nvpair-phoenix|exited"}, removeErr: errors.New("container removal failed")},
		{name: "other-stopped-container", users: []string{"database|exited"}, volumeErr: errors.New("volume is in use")},
		{name: "volume-removal-failure", volumeErr: errors.New("volume removal failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateTelemetryData(t)
			runner := &mockDockerRunner{volumeLabels: map[string]string{"trace": testTraceLabel}, volumeBytes: map[string]int64{"trace": 64}, users: map[string][]string{"trace": tc.users}, usersErr: tc.usersErr, removeErr: tc.removeErr, volumeRemoveErr: tc.volumeErr}
			response := storageRPC(t, storageTestBroker(runner), "telemetry/purge-storage", `{"confirm":"delete-traces"}`)
			if response.Error != nil {
				t.Fatal(response.Error)
			}
			var result purgeWire
			if err := json.Unmarshal(response.Result, &result); err != nil {
				t.Fatal(err)
			}
			if result.Removed == nil || len(result.Removed) != 0 || len(result.Refused) != 1 || result.Refused[0].Name != "trace" || result.Refused[0].Reason == "" {
				t.Fatalf("result = %+v", result)
			}
			if result.Storage.Bytes == nil || *result.Storage.Bytes != 64 {
				t.Errorf("refused storage = %+v", result.Storage)
			}
			if strings.Contains(tc.name, "running") || tc.name == "paused" || tc.name == "malformed-users" || tc.name == "users-failure" {
				if len(runner.removed) != 0 || len(runner.removedVolumes) != 0 {
					t.Errorf("unsafe removal: %v %v", runner.removed, runner.removedVolumes)
				}
			}
			for _, name := range runner.removed {
				if name != "nvpair-phoenix" {
					t.Errorf("removed foreign container %s", name)
				}
			}
		})
	}
}

// The executable fixture tests the real command construction and Docker JSON
// parsing without ever finding or invoking the host's Docker binary.
func TestDockerStorageCommands(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	capture := filepath.Join(dir, "args")
	t.Setenv("DOCKER_TEST_ARGS", capture)
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$DOCKER_TEST_ARGS\"\nprintf '%s' \"$DOCKER_TEST_OUTPUT\"\nexit \"${DOCKER_TEST_EXIT:-0}\"\n"
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &defaultDockerRunner{dockerPath: docker}
	assertArgs := func(want string) {
		t.Helper()
		data, err := os.ReadFile(capture)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != want {
			t.Errorf("args = %q, want %q", data, want)
		}
	}
	t.Setenv("DOCKER_TEST_OUTPUT", "{\"Name\":\"trace\",\"Driver\":\"local\",\"Labels\":\"com.docker.compose.volume=nvpair-phoenix-data\"}\n{\"Name\":\"old_trace\",\"Driver\":\"local\"}\n")
	volumes, err := runner.listVolumes(context.Background(), testTraceLabel)
	if err != nil || len(volumes) != 2 || volumes[0].Name != "trace" || volumes[1].Name != "old_trace" {
		t.Fatalf("volumes = %+v, err = %v", volumes, err)
	}
	assertArgs("volume\nls\n--filter\nlabel=com.docker.compose.volume=nvpair-phoenix-data\n--format\n{{json .}}\n")
	t.Setenv("DOCKER_TEST_OUTPUT", `{"Images":[],"Containers":[],"Volumes":[{"Name":"trace","Size":"1.024kB","Links":"1"},{"Name":"old_trace","Size":"2MB","Links":"0"},{"Name":"empty","Size":"0B"}],"BuildCache":[]}`)
	sizes, err := runner.volumeSizes(context.Background())
	if err != nil || !reflect.DeepEqual(sizes, map[string]int64{"trace": 1024, "old_trace": 2000000, "empty": 0}) {
		t.Fatalf("sizes=%v, err=%v", sizes, err)
	}
	assertArgs("system\ndf\n-v\n--format\n{{json .}}\n")
	t.Setenv("DOCKER_TEST_OUTPUT", "nvpair-phoenix|exited\ndatabase|running\n")
	users, err := runner.volumeUsers(context.Background(), "trace")
	if err != nil || !reflect.DeepEqual(users, []string{"nvpair-phoenix|exited", "database|running"}) {
		t.Fatalf("users=%v, err=%v", users, err)
	}
	assertArgs("ps\n-a\n--filter\nvolume=trace\n--format\n{{.Names}}|{{.State}}\n")
	if err := runner.removeVolume(context.Background(), "trace"); err != nil {
		t.Fatal(err)
	}
	assertArgs("volume\nrm\ntrace\n") // No force, prune, or other volume.
	for _, output := range []string{`garbage`, `{}`, `{"Volumes":[{"Name":"trace","Size":"N/A"}]}`, `{"Volumes":[{"Name":"trace","Size":"-1B"}]}`, `{"Volumes":[{"Name":"trace","Size":"1e50B"}]}`} {
		t.Setenv("DOCKER_TEST_OUTPUT", output)
		if _, err := runner.volumeSizes(context.Background()); err == nil {
			t.Errorf("accepted invalid disk usage: %s", output)
		}
	}
	t.Setenv("DOCKER_TEST_OUTPUT", `{"Volumes":[]}`)
	if sizes, err := runner.volumeSizes(context.Background()); err != nil || len(sizes) != 0 {
		t.Errorf("empty disk usage = %v, %v", sizes, err)
	}
	for _, output := range []string{`garbage`, `{}`, `{"Name":"-unsafe"}`} {
		t.Setenv("DOCKER_TEST_OUTPUT", output)
		if _, err := runner.listVolumes(context.Background(), testTraceLabel); err == nil {
			t.Errorf("accepted invalid volume: %s", output)
		}
	}
	t.Setenv("DOCKER_TEST_EXIT", "1")
	if _, err := runner.listVolumes(context.Background(), testTraceLabel); err == nil {
		t.Error("lost list error")
	}
	if _, err := runner.volumeSizes(context.Background()); err == nil {
		t.Error("lost sizes error")
	}
	if _, err := runner.volumeUsers(context.Background(), "trace"); err == nil {
		t.Error("lost users error")
	}
	if err := runner.removeVolume(context.Background(), "trace"); err == nil {
		t.Error("lost remove error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runner.listVolumes(ctx, testTraceLabel); err == nil {
		t.Error("ignored cancellation")
	}
}

func TestTelemetryPurgeAbortsOnStopOrListFailure(t *testing.T) {
	for _, tc := range []struct {
		name             string
		stopErr, listErr error
	}{
		{"stop", errors.New("stop failed"), nil}, {"list", nil, errors.New("list failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateTelemetryData(t)
			runner := &mockDockerRunner{stopErr: tc.stopErr, listErr: tc.listErr}
			response := storageRPC(t, storageTestBroker(runner), "telemetry/purge-storage", `{"confirm":"delete-traces"}`)
			if response.Error == nil || response.Error.Code != -32000 {
				t.Errorf("want -32000, got %+v", response)
			}
			if len(runner.removed) != 0 || len(runner.removedVolumes) != 0 {
				t.Error("must not delete after prerequisite failure")
			}
		})
	}
}
