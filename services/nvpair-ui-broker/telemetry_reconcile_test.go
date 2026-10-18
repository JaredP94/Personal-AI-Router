// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net"
	"reflect"
	"testing"
	"time"
)

// Keep the real broker request correlation/read pump; only the external worker
// is replaced. A missing configure, wrong policy, or stale response must fail.
func newTelemetryProxyPeer(t *testing.T, handler func(*Codec, *Message)) *proxyProcess {
	t.Helper()
	local, remote := net.Pipe()
	p := &proxyProcess{peer: NewPeer(NewCodec(local)), ready: true}
	t.Cleanup(func() { local.Close(); remote.Close(); p.peer.Close() })
	go p.peer.Serve(nil, p.handleNotify)
	codec := NewCodec(remote)
	go func() {
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			handler(codec, msg)
		}
	}()
	return p
}

func (b *Broker) cacheDesiredTelemetry(config telemetryDesiredConfig) {
	b.telemetryConfigMu.Lock()
	defer b.telemetryConfigMu.Unlock()
	b.cacheDesiredTelemetryLocked(config)
}

func (b *Broker) telemetryWorkerStates() []workerTelemetryState {
	_, states, _ := b.telemetrySnapshot()
	return states
}

func telemetryConfigFixture(revision uint64) telemetryDesiredConfig {
	return telemetryDesiredConfig{Revision: revision, Enabled: true, CollectorPort: 54317, RecordPayloads: true, PayloadConsentVersion: 1, PayloadPolicyVersion: 1}
}

func acceptTelemetryConfig(t *testing.T, seen chan<- telemetryDesiredConfig) func(*Codec, *Message) {
	return func(codec *Codec, msg *Message) {
		if msg.Method != "telemetry/configure" {
			t.Errorf("method = %s", msg.Method)
			codec.RespondError(msg.ID, -32601, "unexpected method")
			return
		}
		var policy struct {
			Revision              *uint64 `json:"revision"`
			Enabled               *bool   `json:"enabled"`
			CollectorPort         *int    `json:"collectorPort"`
			RecordPayloads        *bool   `json:"recordPayloads"`
			PayloadConsentVersion *int    `json:"payloadConsentVersion"`
			PayloadPolicyVersion  *int    `json:"payloadPolicyVersion"`
		}
		err := json.Unmarshal(msg.Params, &policy)
		if err != nil || policy.Revision == nil || policy.Enabled == nil || policy.CollectorPort == nil || policy.RecordPayloads == nil || policy.PayloadConsentVersion == nil || policy.PayloadPolicyVersion == nil {
			t.Errorf("invalid configure: %s", msg.Params)
			codec.RespondError(msg.ID, -32602, "invalid policy")
			return
		}
		seen <- telemetryDesiredConfig{Revision: *policy.Revision, Enabled: *policy.Enabled, CollectorPort: *policy.CollectorPort, RecordPayloads: *policy.RecordPayloads, PayloadConsentVersion: *policy.PayloadConsentVersion, PayloadPolicyVersion: *policy.PayloadPolicyVersion}
		codec.Respond(msg.ID, map[string]uint64{"revision": *policy.Revision})
	}
}

func receiveTelemetryConfig(t *testing.T, ch <-chan telemetryDesiredConfig) telemetryDesiredConfig {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not receive telemetry/configure")
		return telemetryDesiredConfig{}
	}
}

func TestReconcileAppliesDesiredToEveryProxy(t *testing.T) {
	b := &Broker{}
	seen := make(chan telemetryDesiredConfig, 3)
	b.setProxy(newTelemetryProxyPeer(t, acceptTelemetryConfig(t, seen)))
	b.setLMStudioProxy(newTelemetryProxyPeer(t, acceptTelemetryConfig(t, seen)))
	b.setOMLXProxy(newTelemetryProxyPeer(t, acceptTelemetryConfig(t, seen)))
	b.cacheDesiredTelemetry(telemetryConfigFixture(7))
	b.reconcileTelemetry()
	for range 3 {
		got := receiveTelemetryConfig(t, seen)
		if got.Revision != 7 || !got.Enabled || got.CollectorPort != 54317 || !got.RecordPayloads {
			t.Errorf("configure = %+v", got)
		}
	}
	for _, state := range b.telemetryWorkerStates() {
		if state.State != "applied" || state.AppliedRevision != 7 || state.Error != "" {
			t.Errorf("worker = %+v", state)
		}
	}
}

func TestProxyRestartGetsLatestConfig(t *testing.T) {
	for _, name := range []string{"proxy", "lmstudio-proxy", "omlx-proxy"} {
		t.Run(name, func(t *testing.T) {
			b := &Broker{}
			install, notify, index := b.setProxy, b.forwardProxyNotification, 0
			if name == "lmstudio-proxy" {
				install, notify, index = b.setLMStudioProxy, b.forwardLMStudioProxyNotification, 1
			}
			if name == "omlx-proxy" {
				install, notify, index = b.setOMLXProxy, b.forwardOMLXProxyNotification, 2
			}
			b.cacheDesiredTelemetry(telemetryConfigFixture(2))
			seen := make(chan telemetryDesiredConfig, 2)
			install(newTelemetryProxyPeer(t, acceptTelemetryConfig(t, seen)))
			b.reconcileTelemetry()
			receiveTelemetryConfig(t, seen)
			install(nil)
			b.cacheDesiredTelemetry(telemetryConfigFixture(9))
			replacement := newTelemetryProxyPeer(t, acceptTelemetryConfig(t, seen))
			replacement.onNotify = notify
			install(replacement)
			replacement.handleNotify("ready", json.RawMessage(`{"port":0,"version":"test"}`))
			if got := receiveTelemetryConfig(t, seen); got.Revision != 9 {
				t.Fatalf("replacement revision = %d, want 9", got.Revision)
			}
			b.reconcileTelemetry()
			if got := b.telemetryWorkerStates()[index]; got.State != "applied" || got.AppliedRevision != 9 {
				t.Fatalf("replacement = %+v", got)
			}
		})
	}
}

func TestSettingsMissingKeepsProxiesDisabled(t *testing.T) {
	b := &Broker{}
	worker, _ := newTestRPCWorkerPipe(t)
	b.setSettings(worker)
	b.setSettings(nil) // A worker existed, but no configuration was ever loaded.
	seen := make(chan telemetryDesiredConfig, 1)
	b.setProxy(newTelemetryProxyPeer(t, acceptTelemetryConfig(t, seen)))
	b.reconcileTelemetry()
	got := receiveTelemetryConfig(t, seen)
	if got != (telemetryDesiredConfig{CollectorPort: 4317, PayloadPolicyVersion: 1}) {
		t.Fatalf("missing settings policy = %+v", got)
	}
	states := b.telemetryWorkerStates()
	if states[1].State != "unavailable" || states[2].State != "unavailable" {
		t.Fatalf("absent workers = %+v", states)
	}
}

func TestConfigureFailureReportedAsFailed(t *testing.T) {
	for _, kind := range []string{"rpc", "malformed", "wrong revision", "transport"} {
		t.Run(kind, func(t *testing.T) {
			b := &Broker{}
			b.cacheDesiredTelemetry(telemetryConfigFixture(4))
			p := newTelemetryProxyPeer(t, func(codec *Codec, msg *Message) {
				switch kind {
				case "rpc":
					codec.RespondError(msg.ID, -32603, "cannot configure")
				case "malformed":
					codec.Respond(msg.ID, map[string]string{"revision": "bad"})
				case "wrong revision":
					codec.Respond(msg.ID, map[string]uint64{"revision": 3})
				}
			})
			if kind == "transport" {
				p.peer.Close()
			}
			b.setProxy(p)
			b.reconcileTelemetry()
			got := b.telemetryWorkerStates()[0]
			if got.State != "failed" || got.Error == "" {
				t.Fatalf("failure not reported: %+v", got)
			}
		})
	}
}

func TestLateOldReconcileCannotOverwriteNewer(t *testing.T) {
	b := &Broker{}
	entered, release := make(chan struct{}), make(chan struct{})
	seen := make(chan telemetryDesiredConfig, 2)
	b.setProxy(newTelemetryProxyPeer(t, func(codec *Codec, msg *Message) {
		var config telemetryDesiredConfig
		json.Unmarshal(msg.Params, &config)
		if config.Revision == 1 {
			close(entered)
			<-release
		}
		acceptTelemetryConfig(t, seen)(codec, msg)
	}))
	b.cacheDesiredTelemetry(telemetryConfigFixture(1))
	firstDone := make(chan struct{})
	go func() { b.reconcileTelemetry(); close(firstDone) }()
	waitSchedulerTestChannel(t, entered, "old configure was not started")
	b.cacheDesiredTelemetry(telemetryConfigFixture(2))
	secondDone := make(chan struct{})
	go func() { b.reconcileTelemetry(); close(secondDone) }()
	close(release)
	waitSchedulerTestChannel(t, firstDone, "first reconcile stalled")
	waitSchedulerTestChannel(t, secondDone, "second reconcile stalled")
	receiveTelemetryConfig(t, seen)
	if got := receiveTelemetryConfig(t, seen); got.Revision != 2 {
		t.Fatalf("last configure = %+v", got)
	}
	if got := b.telemetryWorkerStates()[0]; got.State != "applied" || got.AppliedRevision != 2 {
		t.Fatalf("stale completion won: %+v", got)
	}
}

func TestStatusReportsDesiredAndEffective(t *testing.T) {
	b := &Broker{dockerRunner: &mockDockerRunner{state: "running", version: "arize-phoenix-v20.16.0"}, dialTimeout: func(network, address string, timeout time.Duration) (net.Conn, error) {
		if network != "tcp" || address != "127.0.0.1:54317" {
			t.Errorf("probe target = %s %s", network, address)
		}
		a, z := net.Pipe()
		z.Close()
		return a, nil
	}}
	b.cacheDesiredTelemetry(telemetryConfigFixture(12))
	seen := make(chan telemetryDesiredConfig, 1)
	b.setProxy(newTelemetryProxyPeer(t, func(codec *Codec, msg *Message) {
		if msg.Method == "telemetry/status" {
			codec.Respond(msg.ID, map[string]interface{}{"revision": 12, "enabled": true, "lastExportOk": "2026-10-18T01:02:03Z", "lastExportError": "delivery failed", "lastExportErrorAt": "2026-10-18T01:02:04Z"})
			return
		}
		acceptTelemetryConfig(t, seen)(codec, msg)
	}))
	b.reconcileTelemetry()
	status := b.getTelemetryStatus(context.Background())
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	json.Unmarshal(raw, &got)
	want := map[string]string{"revision": "12", "enabled": "true", "collectorPort": "54317", "recordPayloads": "true", "payloadConsentVersion": "1", "payloadPolicyVersion": "1", "containerState": `"running"`, "collectorReachable": "true", "uiUrl": `"http://127.0.0.1:6006"`, "imageVersion": `"v20.16.0"`, "bindingsLoopbackOnly": "null", "managedConfig": "null", "storage": "null"}
	if len(got) != len(want)+1 {
		t.Errorf("unexpected status keys: %s", raw)
	}
	for key, value := range want {
		if string(got[key]) != value {
			t.Errorf("%s = %s, want %s", key, got[key], value)
		}
	}
	var workers []map[string]interface{}
	json.Unmarshal(got["workers"], &workers)
	expected := []map[string]interface{}{
		{"name": "proxy", "appliedRevision": float64(12), "state": "applied", "error": "", "lastExportOk": "2026-10-18T01:02:03Z", "lastExportError": "delivery failed", "lastExportErrorAt": "2026-10-18T01:02:04Z"},
		{"name": "lmstudio-proxy", "appliedRevision": float64(0), "state": "unavailable", "error": "", "lastExportOk": nil, "lastExportError": nil, "lastExportErrorAt": nil},
		{"name": "omlx-proxy", "appliedRevision": float64(0), "state": "unavailable", "error": "", "lastExportOk": nil, "lastExportError": nil, "lastExportErrorAt": nil},
	}
	if !reflect.DeepEqual(workers, expected) {
		t.Errorf("workers = %+v, want %+v", workers, expected)
	}
}

func TestTelemetryHealthFailuresLeaveNull(t *testing.T) {
	for _, kind := range []string{"rpc", "malformed", "timeout", "transport"} {
		t.Run(kind, func(t *testing.T) {
			b := &Broker{dockerRunner: &mockDockerRunner{state: "not_found"}, dialTimeout: func(string, string, time.Duration) (net.Conn, error) { return nil, net.ErrClosed }}
			p := newTelemetryProxyPeer(t, func(codec *Codec, msg *Message) {
				switch kind {
				case "rpc":
					codec.RespondError(msg.ID, -32603, "unavailable")
				case "malformed":
					codec.Respond(msg.ID, map[string]interface{}{"lastExportOk": "must not survive failed decoding", "lastExportError": 42})
				}
			})
			b.setProxy(p)
			if kind == "transport" {
				p.peer.Close()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			got := b.getTelemetryStatus(ctx).Workers[0]
			if got.LastExportOK != nil || got.LastExportError != nil || got.LastExportErrorAt != nil {
				t.Fatalf("failed health must be null: %+v", got)
			}
			if got.State != "pending" {
				t.Fatalf("unconfigured worker = %+v", got)
			}
		})
	}
}

func TestLateSettingsReadAndRetiredWorkerCannotReplaceNewerDesired(t *testing.T) {
	b := &Broker{}
	worker, codec := newTestRPCWorkerPipe(t)
	b.setSettings(worker)
	entered, release := make(chan struct{}), make(chan struct{})
	go func() {
		msg, err := codec.Read()
		if err != nil {
			return
		}
		close(entered)
		<-release
		codec.Respond(msg.ID, telemetryConfigFixture(5))
	}()
	done := make(chan struct{})
	go func() { b.loadTelemetryConfig(worker); close(done) }()
	waitSchedulerTestChannel(t, entered, "initial settings read not started")
	raw, _ := json.Marshal(telemetryConfigFixture(6))
	b.forwardSettingsNotificationForWorker(worker, "settings/telemetry-changed", raw)
	close(release)
	waitSchedulerTestChannel(t, done, "initial settings read stalled")
	config, _, _ := b.telemetrySnapshot()
	if config.Revision != 6 {
		t.Fatalf("late snapshot won: %+v", config)
	}
	b.setSettings(nil)
	config, _, _ = b.telemetrySnapshot()
	if config != telemetryConfigFixture(6) {
		t.Fatalf("settings loss policy = %+v", config)
	}
	raw, _ = json.Marshal(telemetryConfigFixture(7))
	b.forwardSettingsNotificationForWorker(worker, "settings/telemetry-changed", raw)
	config, _, _ = b.telemetrySnapshot()
	if config != telemetryConfigFixture(6) {
		t.Fatalf("retired worker changed desired: %+v", config)
	}
	replacement, replacementCodec := newTestRPCWorkerPipe(t)
	b.setSettings(replacement)
	go func() {
		msg, err := replacementCodec.Read()
		if err == nil {
			replacementCodec.Respond(msg.ID, telemetryConfigFixture(8))
		}
	}()
	b.loadTelemetryConfig(replacement)
	config, _, _ = b.telemetrySnapshot()
	if config.Revision != 8 {
		t.Fatalf("settings restart did not reload: %+v", config)
	}
	b.reconcileTelemetry()
}

func TestSettingsNotificationWithoutWorkerCannotEnableTracing(t *testing.T) {
	b := &Broker{}
	raw, _ := json.Marshal(telemetryConfigFixture(7))
	b.forwardSettingsNotification("settings/telemetry-changed", raw)
	config, _, _ := b.telemetrySnapshot()
	if config.Revision != 0 || config.Enabled || config.RecordPayloads {
		t.Fatalf("notification without settings enabled tracing: %+v", config)
	}
	b.reconcileTelemetry()
}

func TestSettingsLossKeepsLastLoadedConfigForRunningAndRestartedProxies(t *testing.T) {
	for _, name := range []string{"proxy", "lmstudio-proxy", "omlx-proxy"} {
		t.Run(name, func(t *testing.T) {
			b := &Broker{}
			install, notify, index := b.setProxy, b.forwardProxyNotification, 0
			if name == "lmstudio-proxy" {
				install, notify, index = b.setLMStudioProxy, b.forwardLMStudioProxyNotification, 1
			}
			if name == "omlx-proxy" {
				install, notify, index = b.setOMLXProxy, b.forwardOMLXProxyNotification, 2
			}
			worker, codec := newTestRPCWorkerPipe(t)
			b.setSettings(worker)
			go func() {
				msg, err := codec.Read()
				if err == nil {
					if msg.Method != "settings/get-telemetry-config" {
						t.Errorf("settings method = %q", msg.Method)
					}
					codec.Respond(msg.ID, telemetryConfigFixture(9))
				}
			}()
			seen := make(chan telemetryDesiredConfig, 4)
			install(newTelemetryProxyPeer(t, acceptTelemetryConfig(t, seen)))
			b.loadTelemetryConfig(worker)
			if got := receiveTelemetryConfig(t, seen); got != telemetryConfigFixture(9) {
				t.Fatalf("loaded configuration = %+v", got)
			}
			b.setSettings(nil)
			b.reconcileTelemetry()
			desired, workers, _ := b.telemetrySnapshot()
			if desired != telemetryConfigFixture(9) {
				t.Errorf("settings loss changed desired configuration: %+v", desired)
			}
			if got := workers[index]; got.State != "applied" || got.AppliedRevision != 9 || got.Error != "" {
				t.Errorf("running proxy after settings loss = %+v", got)
			}
			install(nil)
			replacement := newTelemetryProxyPeer(t, acceptTelemetryConfig(t, seen))
			replacement.onNotify = notify
			install(replacement)
			replacement.handleNotify("ready", json.RawMessage(`{"port":0,"version":"test"}`))
			if got := receiveTelemetryConfig(t, seen); got != telemetryConfigFixture(9) {
				t.Fatalf("replacement configuration while settings is down = %+v", got)
			}
			b.reconcileTelemetry()
			if got := b.telemetryWorkerStates()[index]; got.State != "applied" || got.AppliedRevision != 9 || got.Error != "" {
				t.Fatalf("restarted proxy after settings loss = %+v", got)
			}
		})
	}
}

func TestSettingsReadyLoadsTelemetryAndChangesReconcile(t *testing.T) {
	b := &Broker{}
	worker, codec := newTestRPCWorkerPipe(t)
	b.setSettings(worker)
	go func() {
		msg, err := codec.Read()
		if err != nil {
			return
		}
		if msg.Method != "settings/get-telemetry-config" {
			t.Errorf("settings method = %q", msg.Method)
		}
		codec.Respond(msg.ID, telemetryConfigFixture(5))
	}()
	seen := make(chan telemetryDesiredConfig, 2)
	b.setProxy(newTelemetryProxyPeer(t, acceptTelemetryConfig(t, seen)))
	b.forwardSettingsNotification("ready", nil)
	if got := receiveTelemetryConfig(t, seen); got.Revision != 5 {
		t.Errorf("loaded = %+v", got)
	}
	raw, _ := json.Marshal(telemetryConfigFixture(6))
	b.forwardSettingsNotification("settings/telemetry-changed", raw)
	if got := receiveTelemetryConfig(t, seen); got.Revision != 6 {
		t.Errorf("changed = %+v", got)
	}
	b.reconcileTelemetry()
}
