// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"nvpair-shared/appdir"
	"nvpair-shared/clustertrust"
	settings "nvpair-shared/enginesettings"
	"nvpair-shared/noderec"
)

type settingsReceipt struct {
	Hash     string `json:"hash"`
	Revision uint64 `json:"revision"`
	Phase    string `json:"phase"`
}
type engineSettingsRecord struct {
	Snapshot settings.Snapshot          `json:"snapshot"`
	Previous settings.Config            `json:"previous"`
	Resume   bool                       `json:"resume"`
	Explicit bool                       `json:"explicit"`
	Receipts map[string]settingsReceipt `json:"receipts"`
}
type activeSettingsOperation struct {
	Engine       string
	Port         int
	PreviousPort int
	Callbacks    *sync.WaitGroup
	Context      context.Context
	Proxy        *proxyProcess
}

func settingsID() string                     { var id [16]byte; _, _ = rand.Read(id[:]); return hex.EncodeToString(id[:]) }
func settingsJSON(value any) json.RawMessage { data, _ := json.Marshal(value); return data }

func (b *Broker) engineSettingsPath() (string, error) {
	if base := b.clusterManagerConfigDir(); base != "" {
		return filepath.Join(base, "engine-settings-operations.json"), nil
	}
	return appdir.Path("engine-settings-operations.json")
}

// The journal records accepted desired configuration and operation outcomes.
// Component stores still supply the normal settings baseline. Interrupted
// operations are reconciled; failed applies restore the previous choices.
func (b *Broker) loadEngineSettingsLocked() error {
	if b.engineSettingsLoaded {
		return b.engineSettingsError
	}
	b.engineSettingsLoaded = true
	b.engineSettingsEpoch = settingsID()
	b.engineSettings = make(map[string]*engineSettingsRecord)
	path, err := b.engineSettingsPath()
	if err != nil {
		b.engineSettingsError = err
		return err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err == nil {
		err = json.Unmarshal(data, &b.engineSettings)
	}
	if err != nil || b.engineSettings == nil {
		b.engineSettingsError = fmt.Errorf("settings operation journal could not be read; existing settings were preserved")
		return b.engineSettingsError
	}
	for _, record := range b.engineSettings {
		if record == nil {
			b.engineSettingsError = fmt.Errorf("invalid settings journal")
			return b.engineSettingsError
		}
		record.Snapshot.Epoch = b.engineSettingsEpoch
		record.Snapshot.Sequence = 0
		if record.Receipts == nil {
			record.Receipts = make(map[string]settingsReceipt)
		}
	}
	return nil
}

func (b *Broker) saveEngineSettingsLocked() error {
	// Bound recovery metadata. An evicted retry still has its original stale
	// expected revision, so it cannot overwrite a later configuration.
	for _, record := range b.engineSettings {
		for len(record.Receipts) > 256 {
			oldest := ""
			var revision uint64 = ^uint64(0)
			for id, receipt := range record.Receipts {
				if id != record.Snapshot.RequestID && receipt.Phase != "applying" && receipt.Revision <= revision {
					oldest = id
					revision = receipt.Revision
				}
			}
			if oldest == "" {
				break
			}
			delete(record.Receipts, oldest)
		}
	}
	path, err := b.engineSettingsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(b.engineSettings)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".engine-settings-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err = file.Chmod(0o600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (b *Broker) settingsWorkerCall(ctx context.Context, method string, params any, target any) error {
	// A concurrent snapshot/preview must not hold engineConfigMu beyond the
	// active Apply's work deadline and consume its reserved finalization time.
	if deadline := b.settingsApplyWorkDeadline.Load(); deadline != 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, time.Unix(0, deadline))
		defer cancel()
	}
	w := b.getEngineMgr()
	if w == nil {
		return fmt.Errorf("engine manager unavailable")
	}
	result, rpcErr, err := w.CallNoTimeout(ctx, method, settingsJSON(params))
	if err != nil {
		return err
	}
	if rpcErr != nil {
		return fmt.Errorf("%s", rpcErr.Message)
	}
	if target != nil {
		return json.Unmarshal(result, target)
	}
	return nil
}

func (b *Broker) settingsProxy(engine string) *proxyProcess {
	if engine == "ollama" {
		return b.getProxy()
	}
	if engine == "lmstudio" {
		return b.getLMStudioProxy()
	}
	if engine == "omlx" {
		return b.getOMLXProxy()
	}
	return nil
}

func (b *Broker) settingsSnapshotLocked(ctx context.Context, engine string) (settings.Snapshot, error) {
	if engine != "ollama" && engine != "lmstudio" && engine != "omlx" && engine != "llamacpp" {
		return settings.Snapshot{}, fmt.Errorf("this engine does not support settings")
	}
	if err := b.loadEngineSettingsLocked(); err != nil {
		return settings.Snapshot{}, err
	}
	var launch settings.LaunchState
	if err := b.settingsWorkerCall(ctx, "engine:get-launch", map[string]string{"engine": engine}, &launch); err != nil {
		return settings.Snapshot{}, err
	}
	proxyPort := 0
	ready := engine == "llamacpp" // lifecycle-only; no proxy exists yet
	if proxy := b.settingsProxy(engine); proxy != nil {
		ready, proxyPort = proxy.Status()
	}
	record := b.engineSettings[engine]
	configuredProxy := proxyPort
	if record != nil && (!ready || record.Explicit) {
		configuredProxy = record.Snapshot.Settings.ProxyPort
	}
	current := settings.Config{ServerPort: launch.ServerPort, ProxyPort: configuredProxy, LaunchText: launch.LaunchText}
	if record == nil {
		record = &engineSettingsRecord{Snapshot: settings.Snapshot{NodeID: b.nodeID, Engine: engine, Revision: 1, AppliedRevision: 1, Epoch: b.engineSettingsEpoch, Phase: "idle", Settings: current}, Receipts: make(map[string]settingsReceipt)}
		b.engineSettings[engine] = record
		if err := b.saveEngineSettingsLocked(); err != nil {
			delete(b.engineSettings, engine)
			return settings.Snapshot{}, err
		}
	} else if record.Snapshot.Phase != "applying" && record.Snapshot.Phase != "failed" && record.Snapshot.Settings != current {
		record.Snapshot.Settings = current
		record.Snapshot.Revision++
		record.Snapshot.AppliedRevision = record.Snapshot.Revision
		if err := b.saveEngineSettingsLocked(); err != nil {
			return settings.Snapshot{}, err
		}
	}
	snapshot := record.Snapshot
	snapshot.EffectiveServerPort = launch.EffectivePort
	snapshot.EffectiveProxyPort = proxyPort
	snapshot.Running = launch.Running
	snapshot.Adopted = launch.Adopted
	snapshot.Format = launch.Format
	snapshot.Editable = launch.Editable && ready
	snapshot.Reason = launch.Reason
	if !ready {
		snapshot.Reason = "The proxy service is unavailable."
	}
	if snapshot.Phase == "applying" {
		snapshot.Editable = false
		snapshot.Reason = "Settings are being applied."
	}
	if !reflect.DeepEqual(snapshot, record.Snapshot) {
		snapshot.Sequence++
		record.Snapshot = snapshot
	}
	return snapshot, nil
}

type settingsPublication struct {
	all    []settings.Snapshot
	codec  *Codec
	worker *rpcWorker
}

// Capture under engineConfigMu; the single sender takes immutable projections
// off the lock before either Notify. A slow sink retains at most one pending
// full projection. Coalescing never reverses any engine's sequence.
func (b *Broker) publishSettingsLocked() {
	all := make([]settings.Snapshot, 0, len(b.engineSettings))
	for _, engine := range []string{"ollama", "lmstudio", "omlx", "llamacpp"} {
		if record := b.engineSettings[engine]; record != nil {
			record.Snapshot.Sequence++
			all = append(all, record.Snapshot)
		}
	}
	b.settingsPublication = &settingsPublication{all: all, codec: b.codec, worker: b.getEngineMgr()}
	if !b.settingsPublishing {
		b.settingsPublishing = true
		go b.sendSettingsPublications()
	}
}

func (b *Broker) sendSettingsPublications() {
	for {
		b.engineConfigMu.Lock()
		publication := b.settingsPublication
		b.settingsPublication = nil
		if publication == nil {
			b.settingsPublishing = false
			b.engineConfigMu.Unlock()
			return
		}
		b.engineConfigMu.Unlock()
		if publication.codec != nil {
			for _, snapshot := range publication.all {
				_ = publication.codec.Notify("engine:settings-changed", snapshot)
			}
		}
		if publication.worker != nil {
			_ = publication.worker.Notify("engine:settings-projection", publication.all)
		}
	}
}

func (b *Broker) validateSettingsPortsLocked(ctx context.Context, engine string, config settings.Config, current settings.Snapshot) error {
	if engine == "llamacpp" && config.ProxyPort != 0 {
		return fmt.Errorf("llama.cpp has no PAIR proxy yet; proxy port must be zero")
	}
	if config.ServerPort < 1 || config.ServerPort > 65535 || (engine != "llamacpp" && (config.ProxyPort < 1 || config.ProxyPort > 65535)) {
		return fmt.Errorf("ports must be between 1 and 65535")
	}
	if config.ServerPort == config.ProxyPort {
		return fmt.Errorf("Server port and proxy port collide: both are %d. Choose different ports.", config.ServerPort)
	}
	reserved := map[int]bool{engineManagerHTTPPort: true, engineControlPort: true, nodeInfoHTTPPort: true, errorsHTTPPort: true, workloadHTTPPort: true, clusterManagerHTTPPort: true}
	if alias := b.currentOllamaHostAlias().Port; alias > 0 {
		reserved[alias] = true
	}
	// Include every registered PAIR listener, including services added later.
	if b.regCache != nil {
		for _, service := range b.regCache.Snapshot() {
			if service.Port > 0 && service.Service != engineSettingsService(engine) {
				reserved[service.Port] = true
			}
		}
	}
	if reserved[config.ServerPort] || reserved[config.ProxyPort] {
		return fmt.Errorf("a selected port is reserved by a PAIR service or proxy alias")
	}
	var installed struct {
		Engines []struct {
			Engine string `json:"engine"`
			Port   int    `json:"port"`
		} `json:"engines"`
	}
	if err := b.settingsWorkerCall(ctx, "engine:configured-ports", nil, &installed); err != nil {
		return err
	}
	for _, other := range installed.Engines {
		if other.Engine != engine && other.Port > 0 && (other.Port == config.ServerPort || other.Port == config.ProxyPort) {
			return fmt.Errorf("a selected port is reserved by another configured engine")
		}
	}
	for _, other := range []string{"ollama", "lmstudio", "omlx"} {
		if other == engine {
			continue
		}
		if record := b.engineSettings[other]; record != nil {
			port := record.Snapshot.Settings.ProxyPort
			if port == config.ServerPort || port == config.ProxyPort {
				return fmt.Errorf("a selected port is reserved by another configured proxy")
			}
		}
		if proxy := b.settingsProxy(other); proxy != nil {
			_, port := proxy.Status()
			if port == config.ServerPort || port == config.ProxyPort {
				return fmt.Errorf("a selected port is already used by another proxy")
			}
		}
	}
	for _, port := range []int{config.ServerPort, config.ProxyPort} {
		if port == 0 {
			continue
		}
		if port == current.EffectiveProxyPort || (current.Running && port == current.EffectiveServerPort) {
			continue
		}
		if !tcpPortAvailable(port) {
			return fmt.Errorf("port %d is already in use", port)
		}
	}
	return nil
}

func (b *Broker) previewSettingsLocked(ctx context.Context, p settings.Request) (settings.Preview, error) {
	current, err := b.settingsSnapshotLocked(ctx, p.Engine)
	if err != nil {
		return settings.Preview{}, err
	}
	if current.Revision != p.ExpectedRevision {
		return settings.Preview{}, fmt.Errorf("settings changed on this device; reload before applying")
	}
	var preview settings.Preview
	if err := b.settingsWorkerCall(ctx, "engine:preview-launch", p, &preview); err != nil {
		return preview, err
	}
	preview.Revision = current.Revision
	preview.Rebind = preview.Settings.ProxyPort != current.EffectiveProxyPort
	if preview.Errors == nil {
		preview.Errors = make(map[string]string)
	}
	if !current.Editable {
		preview.Errors["settings"] = current.Reason
	}
	if len(preview.Errors) == 0 && preview.Conflict == nil {
		if err := b.validateSettingsPortsLocked(ctx, p.Engine, preview.Settings, current); err != nil {
			preview.Errors["ports"] = err.Error()
		}
	}
	return preview, nil
}

// runSettingsOperationLocked applies an already-accepted record. The caller
// holds engineConfigMu; it is released around the engine-manager call, which
// stops and restarts the engine and can run for minutes, and re-acquired to
// record the outcome. Holding the journal lock across that call froze every
// other reader — the other engine's editor, the refresh poller — for the whole
// restart. settingsApplyMu, which the caller also holds, is what keeps a
// second apply out of this window.
func (b *Broker) runSettingsOperationLocked(ctx context.Context, engine string, record *engineSettingsRecord) error {
	return b.runSettingsOperationWithJoinLocked(ctx, engine, record, proxyCallTimeout+portFenceTimeout)
}

func (b *Broker) runSettingsOperationWithJoinLocked(ctx context.Context, engine string, record *engineSettingsRecord, joinBudget time.Duration) error {
	service := engineSettingsService(engine)
	if b.regCache != nil && service != "" {
		b.unregisterService(service)
	}
	b.setProxyLocalBackend(b.settingsProxy(engine), engine, record.Snapshot.EffectiveServerPort, false)
	id := settingsID()
	callbackProxy := b.settingsProxy(engine)
	b.settingsRelayMu.Lock()
	if b.activeSettings == nil {
		b.activeSettings = make(map[string]activeSettingsOperation)
	}
	delete(b.settingsStartGrants, engine)
	callbacks := &sync.WaitGroup{}
	callbackCtx, cancelCallbacks := context.WithCancel(ctx)
	defer cancelCallbacks()
	b.activeSettings[id] = activeSettingsOperation{Engine: engine, Port: record.Snapshot.Settings.ProxyPort, PreviousPort: record.Previous.ProxyPort, Callbacks: callbacks, Context: callbackCtx, Proxy: callbackProxy}
	b.settingsRelayMu.Unlock()
	configure := settings.Configure{Engine: engine, Settings: record.Snapshot.Settings, OperationID: id, Resume: record.Resume}

	launch, err := func() (settings.LaunchState, error) {
		b.engineConfigMu.Unlock()
		defer b.engineConfigMu.Lock()
		var launch settings.LaunchState
		err := b.settingsWorkerCall(ctx, "engine:configure-launch", configure, &launch)
		if err != nil {
			// A failed RPC carries no result, so read the observed facts back
			// here too and keep the whole worker round trip off the lock.
			launch = settings.LaunchState{}
			_ = b.settingsWorkerCall(ctx, "engine:get-launch", map[string]string{"engine": engine}, &launch)
		}
		return launch, err
	}()

	// Seal admission and cancel callbacks, then bound their cancellation joins.
	// Notification delivery is not part of the borrowed port operation.
	b.settingsRelayMu.Lock()
	delete(b.activeSettings, id)
	b.settingsRelayMu.Unlock()
	b.engineConfigMu.Unlock()
	cancelCallbacks()
	joined := make(chan struct{})
	go func() { callbacks.Wait(); close(joined) }()
	joinTimer := time.NewTimer(joinBudget)
	select {
	case <-joined:
	case <-joinTimer.C:
		if callbackProxy != nil {
			callbackProxy.retirePortWrites()
		}
		err = fmt.Errorf("proxy callback cancellation could not be confirmed")
	}
	joinTimer.Stop()
	b.engineConfigMu.Lock()

	if err != nil {
		record.Snapshot.Phase = "failed"
		record.Snapshot.Error = err.Error()
		// Never replay a failed candidate on the next startup. The worker's
		// safe error distinguishes completed compensation from failed recovery.
		record.Snapshot.Settings = record.Previous
		record.Resume = false
		record.Snapshot.Running = false
		b.settingsStartupPort(engine, record.Previous.ProxyPort)
	} else {
		record.Snapshot.Phase = "succeeded"
		record.Snapshot.Error = ""
		record.Snapshot.AppliedRevision = record.Snapshot.Revision
	}
	// Refresh observed facts without reusing the pre-operation running state.
	// A failed revision remains failed even when compensation restored service.
	if launch.Engine != "" {
		record.Snapshot.Running = launch.Running
		record.Snapshot.Adopted = launch.Adopted
		record.Snapshot.EffectiveServerPort = launch.EffectivePort
		record.Snapshot.Editable = launch.Editable
		record.Snapshot.Reason = launch.Reason
		if engine == "ollama" {
			b.ollamaBackendPort.Store(int32(launch.EffectivePort))
		} else if engine == "lmstudio" {
			b.lmstudioBackendPort.Store(int32(launch.EffectivePort))
		}
	}
	proxyReady := false
	if proxy := b.settingsProxy(engine); proxy != nil {
		proxyReady, record.Snapshot.EffectiveProxyPort = proxy.Status()
		proxy.readyMu.Lock()
		retired := proxy.portWritesRetired
		proxy.readyMu.Unlock()
		if retired {
			b.finishSettingsRecoveryLocked(engine, false)
		}
	}
	// An unsuccessful apply can leave the previous engine running. Restore
	// discovery from observed runtime state, independent of the apply result.
	if !b.settingsRecoveryBlocked[engine] && proxyReady && launch.Running && launch.EffectivePort > 0 && record.Snapshot.EffectiveProxyPort > 0 && launch.EffectivePort != record.Snapshot.EffectiveProxyPort {
		b.setProxyLocalBackend(b.settingsProxy(engine), engine, launch.EffectivePort, true)
		if b.regCache != nil {
			b.registerService(noderec.RegisterParams{Service: service, Port: record.Snapshot.EffectiveProxyPort})
		}
	}
	if err == nil {
		record.Resume = false
	}
	if receipt, exists := record.Receipts[record.Snapshot.RequestID]; exists {
		receipt.Phase = record.Snapshot.Phase
		record.Receipts[record.Snapshot.RequestID] = receipt
	}
	if saveErr := b.saveEngineSettingsLocked(); saveErr != nil {
		record.Snapshot.Phase = "failed"
		record.Snapshot.Error = "The operation result could not be saved. Reload to reconcile the device."
		if receipt, exists := record.Receipts[record.Snapshot.RequestID]; exists {
			receipt.Phase = "failed"
			record.Receipts[record.Snapshot.RequestID] = receipt
		}
		err = saveErr
	}
	b.publishSettingsLocked()
	return err
}

// authorizeSettingsCallerLocked rejects a peer this node no longer pins. A
// local caller passes an empty caller and is always allowed.
func (b *Broker) authorizeSettingsCallerLocked(caller string) error {
	if caller == "" {
		return nil
	}
	if !clustertrust.Open(b.clusterDir).HasPin(caller) {
		return fmt.Errorf("the requesting device is no longer paired")
	}
	return nil
}

// getEngineSettings reads the authoritative snapshot for one engine and
// republishes it, so a reader and every subscriber see the same revision.
func (b *Broker) getEngineSettings(ctx context.Context, p settings.Request, caller string) (settings.Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, settings.CallBudget)
	defer cancel()
	b.engineConfigMu.Lock()
	defer b.engineConfigMu.Unlock()
	if err := ctx.Err(); err != nil {
		return settings.Snapshot{}, err
	}
	if err := b.authorizeSettingsCallerLocked(caller); err != nil {
		return settings.Snapshot{}, err
	}
	snapshot, err := b.settingsSnapshotLocked(ctx, p.Engine)
	if err != nil {
		return settings.Snapshot{}, err
	}
	b.publishSettingsLocked()
	return snapshot, nil
}

// previewEngineSettings validates a proposed change and reports what applying
// it would do, without touching the journal or the engine.
func (b *Broker) previewEngineSettings(ctx context.Context, p settings.Request, caller string) (settings.Preview, error) {
	ctx, cancel := context.WithTimeout(ctx, settings.CallBudget)
	defer cancel()
	b.engineConfigMu.Lock()
	defer b.engineConfigMu.Unlock()
	if err := ctx.Err(); err != nil {
		return settings.Preview{}, err
	}
	if err := b.authorizeSettingsCallerLocked(caller); err != nil {
		return settings.Preview{}, err
	}
	return b.previewSettingsLocked(ctx, p)
}

// applyEngineSettings accepts a change into the journal and then converges the
// engine onto it. RequestID makes it idempotent: replaying one returns the
// original receipt, and reusing it for different settings is refused.
func (b *Broker) applyEngineSettings(ctx context.Context, p settings.Request, caller string) (settingsReceipt, error) {
	return b.applyEngineSettingsWithinBudget(ctx, p, caller, settingsApplyTiming{
		total: settings.ApplyBudget, execution: settings.OperationBudget,
		cleanup: 2 * (proxyCallTimeout + portFenceTimeout),
	})
}

// Unexported timing seam keeps deadline regressions testable without real minutes.
// Admission may consume only the time left after reserving execution (including
// worker rollback) and cleanup (callback join plus fenced final convergence).
type settingsApplyTiming struct{ total, execution, cleanup time.Duration }

func lockSettingsWithin(ctx context.Context, mu *sync.Mutex) error {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if mu.TryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (b *Broker) applyEngineSettingsWithinBudget(ctx context.Context, p settings.Request, caller string, timing settingsApplyTiming) (receipt settingsReceipt, resultErr error) {
	deadline := time.Now().Add(timing.total)
	admissionCtx, cancel := context.WithDeadline(ctx, deadline.Add(-timing.execution-timing.cleanup))
	defer cancel()
	op := b.portOperation(p.Engine)
	if err := lockSettingsWithin(admissionCtx, &op.mu); err != nil {
		return settingsReceipt{}, err
	}
	// Retain cross-engine validation/acceptance ordering through convergence.
	if err := lockSettingsWithin(admissionCtx, &b.settingsApplyMu); err != nil {
		op.mu.Unlock() // No journal admission or mutation: nothing to converge.
		return settingsReceipt{}, err
	}
	defer b.settingsApplyMu.Unlock()
	b.settingsApplyWorkDeadline.Store(deadline.Add(-timing.cleanup).UnixNano())
	defer b.settingsApplyWorkDeadline.Store(0)
	// Convergence's independent cancellation fence is also inside the one budget.
	fenceBudget := min(portFenceTimeout, timing.cleanup/4)
	convergenceCtx, cancelConvergence := context.WithDeadline(context.Background(), deadline.Add(-fenceBudget))
	defer cancelConvergence()
	finish := b.ownPortOperationWithin(convergenceCtx, p.Engine, op, fenceBudget)
	journalAdmitted := false
	defer func() {
		if !journalAdmitted {
			op.active.Store(false)
			op.mu.Unlock()
			return
		}
		if err := finish(); err != nil {
			resultErr = fmt.Errorf("settings convergence could not be confirmed")
		}
	}()
	if err := lockSettingsWithin(admissionCtx, &b.engineConfigMu); err != nil {
		return settingsReceipt{}, err
	}
	journalAdmitted = true
	defer b.engineConfigMu.Unlock()
	ctx = admissionCtx
	if err := ctx.Err(); err != nil {
		return settingsReceipt{}, err
	}
	if err := b.authorizeSettingsCallerLocked(caller); err != nil {
		return settingsReceipt{}, err
	}
	if p.RequestID == "" || len(p.RequestID) > 128 {
		return settingsReceipt{}, fmt.Errorf("a request identifier is required")
	}
	if err := b.loadEngineSettingsLocked(); err != nil {
		return settingsReceipt{}, err
	}
	fingerprint := sha256.Sum256(settingsJSON(struct {
		Caller  string
		Request settings.Request
	}{caller, p}))
	hash := hex.EncodeToString(fingerprint[:])
	if record := b.engineSettings[p.Engine]; record != nil {
		if receipt, exists := record.Receipts[p.RequestID]; exists {
			if receipt.Hash != hash {
				return settingsReceipt{}, fmt.Errorf("request identifier was already used for different settings")
			}
			return receipt, nil
		}
	}
	preview, err := b.previewSettingsLocked(ctx, p)
	if err != nil {
		return settingsReceipt{}, err
	}
	if err := preview.ValidationError(); err != nil {
		return settingsReceipt{}, err
	}
	record := b.engineSettings[p.Engine]
	previous := *record
	previous.Receipts = make(map[string]settingsReceipt, len(record.Receipts))
	for k, v := range record.Receipts {
		previous.Receipts[k] = v
	}
	if preview.Settings == record.Snapshot.Settings && record.Snapshot.Phase != "failed" {
		record.Snapshot.RequestID = p.RequestID
		record.Snapshot.Phase = "succeeded"
		record.Receipts[p.RequestID] = settingsReceipt{Hash: hash, Revision: record.Snapshot.Revision, Phase: "succeeded"}
		if err := b.saveEngineSettingsLocked(); err != nil {
			*record = previous
			return settingsReceipt{}, err
		}
		b.publishSettingsLocked()
		return record.Receipts[p.RequestID], nil
	}
	record.Previous = record.Snapshot.Settings
	record.Resume = record.Resume || preview.Restart
	record.Explicit = true
	record.Snapshot.Settings = preview.Settings
	record.Snapshot.Revision++
	record.Snapshot.RequestID = p.RequestID
	record.Snapshot.Phase = "applying"
	record.Snapshot.Error = ""
	record.Receipts[p.RequestID] = settingsReceipt{Hash: hash, Revision: record.Snapshot.Revision, Phase: "applying"}
	if err := b.saveEngineSettingsLocked(); err != nil {
		*record = previous
		return settingsReceipt{}, fmt.Errorf("settings could not be saved; no runtime changes were made")
	}
	b.publishSettingsLocked()
	// Once accepted, completion is target-owned. Losing the requesting UI or
	// peer cannot abandon half a port swap. Keep the ORIGINAL admission deadline,
	// reserving the last twenty seconds for joins and fenced convergence.
	operationCtx, operationCancel := context.WithDeadline(context.Background(), deadline.Add(-timing.cleanup))
	defer operationCancel()
	if err := b.runSettingsOperationWithJoinLocked(operationCtx, p.Engine, record, min(proxyCallTimeout+portFenceTimeout, timing.cleanup/2)); err == nil && b.settingsRecoveryBlocked[p.Engine] {
		b.finishSettingsRecoveryLocked(p.Engine, true)
	}
	return record.Receipts[p.RequestID], nil
}

// dispatchEngineSettings routes an RPC method name to its typed entry point.
// It exists only for the two edges that receive the method as a string — the
// client RPC surface and the peer relay — so no in-process caller has to
// recover a concrete result from an interface.
func (b *Broker) dispatchEngineSettings(ctx context.Context, method string, p settings.Request, caller string) (any, error) {
	switch method {
	case "get":
		return b.getEngineSettings(ctx, p, caller)
	case "preview":
		return b.previewEngineSettings(ctx, p, caller)
	case "apply":
		return b.applyEngineSettings(ctx, p, caller)
	default:
		return nil, fmt.Errorf("unsupported settings method")
	}
}

func (b *Broker) handleEngineSettings(msg *Message) {
	var p settings.Request
	if json.Unmarshal(msg.Params, &p) != nil {
		_ = b.codec.RespondError(msg.ID, -32602, "invalid settings request")
		return
	}
	if p.NodeID != "" && p.NodeID != b.nodeID {
		remote := "engine:remote-" + strings.TrimPrefix(msg.Method, "engine:")
		worker := b.getEngineMgr()
		if worker == nil {
			_ = b.codec.RespondError(msg.ID, -32000, "engine manager unavailable")
			return
		}
		err := worker.RelayRequest(remote, msg.Params, func(result json.RawMessage, rpcErr *RPCError, err error) {
			if err != nil {
				_ = b.codec.RespondError(msg.ID, -32000, err.Error())
			} else if rpcErr != nil {
				_ = b.codec.RespondError(msg.ID, rpcErr.Code, rpcErr.Message)
			} else {
				_ = b.codec.Respond(msg.ID, result)
			}
		})
		if err != nil {
			_ = b.codec.RespondError(msg.ID, -32000, err.Error())
		}
		return
	}
	method := strings.TrimSuffix(strings.TrimPrefix(msg.Method, "engine:"), "-settings")
	result, err := b.dispatchEngineSettings(context.Background(), method, p, "")
	if err != nil {
		_ = b.codec.RespondError(msg.ID, -32000, err.Error())
	} else {
		_ = b.codec.Respond(msg.ID, result)
	}
}

func (b *Broker) rebindSettingsProxy(ctx context.Context, engine string, port int) error {
	if err := b.rebindSettingsProxyOn(ctx, b.settingsProxy(engine), engine, port); err != nil {
		return err
	}
	b.settingsStartupPort(engine, port)
	return nil
}

func (b *Broker) rebindSettingsProxyOn(ctx context.Context, p *proxyProcess, engine string, port int) error {
	if engine == "llamacpp" {
		if p != nil || port != 0 {
			return fmt.Errorf("llama.cpp has no PAIR proxy yet")
		}
		return ctx.Err()
	}
	if p == nil {
		return fmt.Errorf("proxy unavailable")
	}
	// Disable automatic facade takeover before the ready event can race the
	// explicit rebind. The accepted journal restores these choices after restart.
	b.setExplicitSettings(engine, true)
	b.disableManagedSettingsFacade(engine)
	_, rpcErr, err := writeProxyPort(ctx, p, settingsJSON(map[string]int{"port": port}))
	if err != nil {
		return err
	}
	if rpcErr != nil {
		return fmt.Errorf("%s", rpcErr.Message)
	}
	return ctx.Err()
}

func (b *Broker) refreshEngineSettings(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !b.managedPortOwnershipReady() {
				continue
			}
			func() {
				b.engineConfigMu.Lock()
				defer b.engineConfigMu.Unlock()
				readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				changed := false
				for _, engine := range []string{"ollama", "lmstudio", "omlx", "llamacpp"} {
					var before settings.Snapshot
					if r := b.engineSettings[engine]; r != nil {
						before = r.Snapshot
					}
					after, err := b.settingsSnapshotLocked(readCtx, engine)
					if err == nil && before != after {
						changed = true
					}
				}
				if changed {
					b.publishSettingsLocked()
				}
			}()
		}
	}
}

func engineSettingsService(engine string) noderec.ServiceKey {
	switch engine {
	case "ollama":
		return noderec.ServiceOllama
	case "lmstudio":
		return noderec.ServiceLMStudio
	case "omlx":
		return noderec.ServiceOMLX
	default:
		return ""
	}
}

func (b *Broker) setExplicitSettings(engine string, value bool) {
	switch engine {
	case "ollama":
		b.ollamaExplicitSettings.Store(value)
	case "lmstudio":
		b.lmstudioExplicitSettings.Store(value)
	case "omlx":
		b.omlxExplicitSettings.Store(value)
	}
}

func (b *Broker) disableManagedSettingsFacade(engine string) {
	switch engine {
	case "ollama":
		b.managedOllamaFacade.Store(false)
		b.managedOllamaBackend.Store(0)
	case "lmstudio":
		b.managedLMStudioFacade.Store(false)
	}
}

func (b *Broker) settingsStartupPort(engine string, port int) {
	switch engine {
	case "ollama":
		b.ollamaProxyStartupPort.Store(int32(port))
	case "lmstudio":
		b.lmstudioProxyStartupPort.Store(int32(port))
	case "omlx":
		b.omlxProxyStartupPort.Store(int32(port))
	}
}
