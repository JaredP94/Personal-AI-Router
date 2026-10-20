// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"nvpair-shared/workerhealth"
)

// WorkersResult is a complete, startup-ordered lifecycle snapshot.
type WorkersResult struct {
	Workers []WorkerStatus `json:"workers"`
}

type workerEntry struct {
	status       WorkerStatus
	path         *string
	expectedPath string
	sup          *supervisor
	started      bool // client-loop/startup owned; protected by workerStatusMu for shutdown
}

func (b *Broker) initWorkerRegistry() {
	specs := []struct {
		name, binary string
		path         *string
		target       **supervisor
		spawn        func() (supervisedHandle, error)
		clear        func()
	}{
		{"scanner", "nvpair-node-scanner", &b.scannerPath, &b.scannerSup, b.spawnScanner, func() { b.setScanner(nil) }},
		{"errors", "nvpair-errors", &b.errorsPath, &b.errorsSup, b.spawnErrors, func() { b.setErrors(nil) }},
		{"settings", "nvpair-node-settings", &b.settingsPath, &b.settingsSup, b.spawnSettings, func() { b.setSettings(nil) }},
		{"engine-manager", "nvpair-engine-manager", &b.engineMgrPath, &b.engineMgrSup, b.spawnEngineMgr, func() { b.setEngineMgr(nil); b.clearEngineRelaySub() }},
		{"node-info", "nvpair-node-info", &b.nodeInfoPath, &b.nodeInfoSup, b.spawnNodeInfo, func() { b.setNodeInfo(nil) }},
		{"proxy", "ollama-proxy", &b.proxyPath, &b.proxySup, b.spawnProxy, func() { b.setProxy(nil) }},
		{"lmstudio-proxy", "lmstudio-proxy", &b.lmstudioProxyPath, &b.lmstudioProxySup, b.spawnLMStudioProxy, func() { b.setLMStudioProxy(nil) }},
		{"omlx-proxy", "omlx-proxy", &b.omlxProxyPath, &b.omlxProxySup, b.spawnOMLXProxy, func() { b.setOMLXProxy(nil) }},
		{"workload-manager", "nvpair-workload-manager", &b.workloadMgrPath, &b.workloadMgrSup, b.spawnWorkloadManager, func() { b.setWorkloadMgr(nil) }},
		{"manual-nodes", "nvpair-manual-nodes", &b.manualNodesPath, &b.manualNodesSup, b.spawnManualNodes, b.clearManualNodesState},
		{"cluster-manager", "nvpair-cluster-manager", &b.clusterMgrPath, &b.clusterMgrSup, b.spawnClusterManager, func() { b.setClusterMgr(nil) }},
		{"scheduler", "nvpair-job-scheduler", &b.schedulerPath, &b.schedulerSup, b.spawnJobScheduler, func() { b.setScheduler(nil) }},
	}
	interval := b.workerHealthInterval
	if interval == 0 {
		interval = 30 * time.Second
	}
	for _, spec := range specs {
		binary := spec.binary
		if runtime.GOOS == "windows" {
			binary += ".exe"
		}
		expected, err := filepath.Abs(binary)
		if err != nil {
			expected = binary
		}
		eligible := spec.name != "engine-manager" && spec.name != "proxy" && spec.name != "lmstudio-proxy" && spec.name != "omlx-proxy"
		status := WorkerStatus{Name: spec.name, State: WorkerRestarting, RestartOnHang: eligible, Since: time.Now()}
		if *spec.path == "" && spec.name != "scanner" {
			status.State = WorkerMissing
			status.LastError = "binary not found: " + expected
		}
		sup := newSupervisor(spec.name, defaultRestartPolicy(), spec.spawn)
		sup.hang = hangPolicy{interval: interval, deadline: min(5*time.Second, interval), restart: eligible, probe: func(ctx context.Context) error { return b.probeWorker(ctx, spec.name) }}
		sup.onState = b.updateWorker
		sup.onCrash, sup.onRecovered = b.supervisedWorkerCallbacks(spec.name, spec.clear)
		*spec.target = sup
		b.workerEntries = append(b.workerEntries, &workerEntry{status: status, path: spec.path, expectedPath: expected, sup: sup})
	}
	b.errorsSup.onCrash = b.errorsCrashFallback
	b.errorsSup.onRecovered = func() { slog.Info("nvpair-errors recovered") }
	b.configureLMStudioProxySupervisorCallbacks(b.lmstudioProxySup)
	b.configureOMLXProxySupervisorCallbacks(b.omlxProxySup)
	b.proxySup.onExhausted = func(int) { b.finishOllamaProxyTerminal() }
}

func (b *Broker) finishOllamaProxyTerminal() {
	b.disableOllamaHostAliasReservation()
	if b.managedOllamaFacade.Load() {
		b.blockManagedOllamaFacade("the Ollama proxy is unavailable")
	} else {
		b.markOllamaPortReady()
	}
}

// startWorker is startup/client-loop owned. Missing binaries never establish a
// monitor and therefore cannot spin; every resolved auxiliary uses one budget.
func (b *Broker) startWorker(name string) {
	for _, entry := range b.workerEntries {
		if entry.sup.name != name {
			continue
		}
		if *entry.path == "" {
			return
		}
		b.workerStatusMu.Lock()
		entry.started = true
		b.workerStatusMu.Unlock()
		entry.sup.StartRetrying()
		return
	}
}

func (b *Broker) workerActive(name string) bool {
	b.workerStatusMu.Lock()
	defer b.workerStatusMu.Unlock()
	for _, entry := range b.workerEntries {
		if entry.sup.name == name {
			return entry.started
		}
	}
	return true // fixtures that construct only the inference stack
}

func (b *Broker) stopWorkers() {
	for i := len(b.workerEntries) - 1; i >= 0; i-- {
		entry := b.workerEntries[i]
		if b.workerActive(entry.sup.name) {
			entry.sup.Stop()
		}
	}
}

func (b *Broker) workerSnapshotLocked() WorkersResult {
	result := WorkersResult{Workers: make([]WorkerStatus, 0, len(b.workerEntries))}
	for _, entry := range b.workerEntries {
		result.Workers = append(result.Workers, entry.status)
	}
	return result
}
func (b *Broker) workerSnapshot() WorkersResult {
	b.workerStatusMu.Lock()
	defer b.workerStatusMu.Unlock()
	return b.workerSnapshotLocked()
}
func (b *Broker) updateWorker(status WorkerStatus) {
	b.workerStatusMu.Lock()
	defer b.workerStatusMu.Unlock()
	for _, entry := range b.workerEntries {
		if entry.status.Name != status.Name {
			continue
		}
		if entry.status == status {
			return
		}
		entry.status = status
		// Serialize snapshot creation AND publication so concurrent monitor callbacks
		// cannot deliver an older full snapshot after a newer one. No subscription gate.
		if err := b.codec.Notify("workers:update", b.workerSnapshotLocked()); err != nil {
			slog.Debug("relay workers:update failed", "err", err)
		}
		return
	}
}
func (b *Broker) restartWorker(name string) (WorkerStatus, *RPCError) {
	for _, entry := range b.workerEntries {
		if entry.sup.name != name {
			continue
		}
		b.workerStatusMu.Lock()
		status := entry.status
		started := entry.started
		b.workerStatusMu.Unlock()
		if !started {
			if _, err := os.Stat(entry.expectedPath); err != nil {
				return status, nil
			}
			*entry.path = entry.expectedPath
			status.State, status.LastError, status.Since = WorkerRestarting, "", time.Now()
			b.updateWorker(status)
			b.workerStatusMu.Lock()
			entry.started = true
			b.workerStatusMu.Unlock()
			// Initialization can itself call a worker. Keep it off the serial
			// client loop just like the monitor's Stop escalation.
			go entry.sup.StartRetrying()
		} else {
			// Retry only enqueues the request: Stop's escalation must never block the
			// serial client loop. The monitor publishes subsequent lifecycle changes.
			entry.sup.Retry()
		}
		b.workerStatusMu.Lock()
		status = entry.status
		b.workerStatusMu.Unlock()
		return status, nil
	}
	return WorkerStatus{}, &RPCError{Code: -32602, Message: "unknown worker name"}
}

func (b *Broker) handleWorkersRestart(msg *Message) {
	var params struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil || params.Name == "" {
		_ = b.codec.RespondError(msg.ID, -32602, "name is required")
		return
	}
	status, rpcErr := b.restartWorker(params.Name)
	if rpcErr != nil {
		_ = b.codec.RespondError(msg.ID, rpcErr.Code, rpcErr.Message)
		return
	}
	_ = b.codec.Respond(msg.ID, status)
}

// Peer.Call is deadline-aware even with a blocked stdin pipe. Domain relays keep
// their existing (potentially unlimited) request lifetime.
func healthCall(ctx context.Context, peer *Peer) error {
	if peer == nil {
		return fmt.Errorf("worker peer unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, rpcErr, err := peer.Call(ctx, workerhealth.Method, nil)
	if err != nil {
		return err
	}
	if rpcErr != nil {
		return fmt.Errorf("worker health RPC failed: %d", rpcErr.Code)
	}
	var result workerhealth.Result
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("worker health not OK")
	}
	return nil
}

func (b *Broker) probeWorker(ctx context.Context, name string) error {
	var health func(context.Context) error
	b.workersMu.Lock()
	switch name {
	case "scanner":
		if b.scanner != nil {
			health = b.scanner.Health
		}
	case "errors":
		if b.errorsProc != nil {
			health = b.errorsProc.Health
		}
	case "settings":
		if b.settings != nil {
			health = b.settings.Health
		}
	case "engine-manager":
		if b.engineMgr != nil {
			health = b.engineMgr.Health
		}
	case "node-info":
		if b.nodeInfo != nil {
			health = b.nodeInfo.Health
		}
	case "proxy":
		if b.proxy != nil {
			health = b.proxy.Health
		}
	case "lmstudio-proxy":
		if b.lmstudioProxy != nil {
			health = b.lmstudioProxy.Health
		}
	case "omlx-proxy":
		if b.omlxProxy != nil {
			health = b.omlxProxy.Health
		}
	case "workload-manager":
		if b.workloadMgr != nil {
			health = b.workloadMgr.Health
		}
	case "manual-nodes":
		if b.manualNodes != nil {
			health = b.manualNodes.Health
		}
	case "cluster-manager":
		if b.clusterMgr != nil {
			health = b.clusterMgr.Health
		}
	case "scheduler":
		if b.scheduler != nil {
			health = b.scheduler.Health
		}
	}
	b.workersMu.Unlock()
	if health == nil {
		return fmt.Errorf("worker handle unavailable")
	}
	return health(ctx)
}
