// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"nvpair-shared/appdir"
	"nvpair-shared/enginenative"
	"nvpair-shared/noderec"
)

// manualNodeStatus is the subset of nvpair-manual-nodes' ManualNodeStatus the
// broker needs. Its JSON tags match the producer's so node/discovered|
// updated|removed payloads unmarshal straight into it (the GPU/CPU/memory
// sub-objects reuse the broker's discovery types, whose tags are identical).
// The ollama_* / lmstudio_* fields drive the per-engine manual→proxy bridge
// (bridgeManualNode); the rest project into the discovery store via
// manualToEnriched.
type manualNodeStatus struct {
	Kind                 string                      `json:"kind"`
	FromCluster          bool                        `json:"from_cluster,omitempty"`
	IdentityVerification string                      `json:"identityVerification,omitempty"`
	PeerRecordID         string                      `json:"peerRecordId,omitempty"`
	PeerMetadata         *noderec.PeerMetadataParams `json:"peerMetadata,omitempty"`
	ID                   string                      `json:"id"`
	Address              string                      `json:"address"`
	OllamaUp             bool                        `json:"ollama_up"`
	OllamaPort           int                         `json:"ollama_port"`
	OllamaModels         []string                    `json:"ollama_models,omitempty"`
	LMStudioUp           bool                        `json:"lmstudio_up"`
	LMStudioPort         int                         `json:"lmstudio_port"`
	LMStudioModels       []string                    `json:"lmstudio_models,omitempty"`
	OMLXUp               bool                        `json:"omlx_up"`
	LlamaCPPUp           bool                        `json:"llamacpp_up"`
	OMLXPort             int                         `json:"omlx_port"`
	LlamaCPPPort         int                         `json:"llamacpp_port"`
	OMLXModels           []string                    `json:"omlx_models,omitempty"`
	LlamaCPPModels       []string                    `json:"llamacpp_models,omitempty"`
	NodeInfoUp           bool                        `json:"node_info_up"`
	NodeInfoPort         int                         `json:"node_info_port"`
	GPUs                 []GPUInfo                   `json:"gpus"`
	CPU                  *CPUInfo                    `json:"cpu"`
	Memory               *MemoryInfo                 `json:"memory"`
	TelemetryValid       bool                        `json:"telemetryValid"`
	MSSince              int64                       `json:"msSince"`
	SampleSeq            uint64                      `json:"sampleSeq,omitempty"`
	// HostUUID is the remote's stable per-host identity, learned from its
	// node-info /v1/node-info. It lets a manual node key by the same permanent
	// identity as mDNS-discovered nodes (and dedup with itself when the same
	// machine is also discovered). Empty until the node-info probe succeeds.
	HostUUID string `json:"hostUuid,omitempty"`
	// ClusterUUID is the remote's cluster principal.
	ClusterUUID            string                               `json:"clusterUuid,omitempty"`
	Models                 []string                             `json:"models,omitempty"`
	ModelsByEngine         map[string][]string                  `json:"modelsByEngine,omitempty"`
	LoadedByEngine         map[string][]string                  `json:"loadedByEngine,omitempty"`
	DownloadedByEngine     map[string][]noderec.DownloadedModel `json:"downloadedByEngine,omitempty"`
	DownloadErrorsByEngine map[string]string                    `json:"downloadErrorsByEngine,omitempty"`
	// EngineNative is the remote engine's own report of itself, as the
	// manual-node prober read it over the peer's pinned mTLS path. It is
	// the same field a discovered node's record carries, so a manual node
	// and a discovered node present the engine's view identically.
	EngineNative *enginenative.Stats `json:"engineNative,omitempty"`
}

type manualNodeStatusEntry struct {
	generation uint64
	status     manualNodeStatus
	receivedAt time.Time
}

type manualNodeTelemetryParams struct {
	PeerRecordID      string `json:"peerRecordId,omitempty"`
	ID                string `json:"id"`
	HostUUID          string `json:"hostUuid,omitempty"`
	GPUUtilizationPct uint32 `json:"gpuUtilizationPct"`
	TelemetryValid    bool   `json:"telemetryValid"`
	MSSince           int64  `json:"msSince"`
	SampleSeq         uint64 `json:"sampleSeq,omitempty"`
}

type persistedManualNode struct {
	Kind string `json:"kind"`
	// PeerUUID is a previously proven association used only to forget this
	// address on unpair. It is never used as inventory or a routing proof.
	PeerUUID string `json:"peer_uuid,omitempty"`
	ID       string `json:"id,omitempty"`
	Address  string `json:"address"`
	Name     string `json:"name,omitempty"`
	TLSPort  int    `json:"tls_port,omitempty"`
	MTLS     bool   `json:"mtls,omitempty"`
}

func (e persistedManualNode) key() string {
	if e.ID != "" {
		return e.ID
	}
	if e.Name != "" {
		return e.Name
	}
	return "manual:" + e.Address
}

func defaultManualNodesConfigPath() string {
	if path, err := appdir.Path("configs", "manual-nodes.json"); err == nil {
		return path
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "configs", "manual-nodes.json")
	}
	return filepath.Join("configs", "manual-nodes.json")
}

func loadPersistedManualNodes(path string) ([]persistedManualNode, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []persistedManualNode
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	valid := entries[:0]
	migrated := false
	for _, entry := range entries {
		if entry.Address == "" {
			continue
		}
		if entry.Kind == "" {
			// Historical files cannot prove user consent to the engine bypass.
			entry.Kind = "pair"
			migrated = true
		}
		if entry.Kind != "pair" && entry.Kind != "engine" {
			return nil, fmt.Errorf("invalid persisted manual node kind %q", entry.Kind)
		}
		valid = append(valid, entry)
	}
	if migrated {
		if err := writePersistedManualNodes(path, valid); err != nil {
			return nil, err
		}
	}
	return valid, nil
}

func writePersistedManualNodes(path string, entries []persistedManualNode) error {
	sort.Slice(entries, func(i, j int) bool { return entries[i].key() < entries[j].key() })
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (b *Broker) persistManualNode(entry persistedManualNode) error {
	if entry.Kind == "" {
		entry.Kind = "pair"
	}
	if entry.Kind != "pair" && entry.Kind != "engine" {
		return fmt.Errorf("invalid manual node kind")
	}
	// The client cannot choose a proven peer association.
	entry.PeerUUID = ""
	b.manualMu.Lock()
	status := b.manualNodeStatuses[entry.key()].status
	b.manualMu.Unlock()
	if entry.Kind == "pair" && status.Address == entry.Address && status.IdentityVerification == "verified" {
		entry.PeerUUID = status.HostUUID
	}
	if entry.ID == "" {
		entry.ID = entry.key()
	}
	b.manualPersistMu.Lock()
	defer b.manualPersistMu.Unlock()
	entries, err := loadPersistedManualNodes(b.manualNodesConfigPath)
	if err != nil {
		return err
	}
	replaced := false
	for i := range entries {
		if entries[i].key() == entry.key() {
			if entry.Kind == "pair" && entries[i].Kind == "pair" && entries[i].Address == entry.Address && entries[i].PeerUUID != "" {
				entry.PeerUUID = entries[i].PeerUUID
			}
			entries[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		entries = append(entries, entry)
	}
	return writePersistedManualNodes(b.manualNodesConfigPath, entries)
}

func (b *Broker) removePersistedManualNode(id string) error {
	b.manualPersistMu.Lock()
	defer b.manualPersistMu.Unlock()
	entries, err := loadPersistedManualNodes(b.manualNodesConfigPath)
	if err != nil {
		return err
	}
	out := entries[:0]
	for _, entry := range entries {
		if entry.key() != id && entry.ID != id && entry.Name != id {
			out = append(out, entry)
		}
	}
	return writePersistedManualNodes(b.manualNodesConfigPath, out)
}

func (b *Broker) replayPersistedManualNodes(worker *rpcWorker) {
	b.manualMutationMu.Lock()
	defer b.manualMutationMu.Unlock()
	b.manualPersistMu.Lock()
	entries, err := loadPersistedManualNodes(b.manualNodesConfigPath)
	b.manualPersistMu.Unlock()
	if err != nil {
		slog.Warn("failed to load persisted manual nodes", "err", err)
		return
	}
	for _, entry := range entries {
		params, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		if _, rpcErr, err := worker.Call(context.Background(), "node/add", params); err != nil {
			slog.Warn("failed to replay manual node", "id", entry.key(), "err", err)
		} else if rpcErr != nil {
			slog.Warn("manual node replay rejected", "id", entry.key(), "code", rpcErr.Code, "msg", rpcErr.Message)
		}
	}
}

func (b *Broker) manualAliasForKey(id string) string {
	b.manualMu.Lock()
	defer b.manualMu.Unlock()
	if _, ok := b.manualNodeKeys[id]; ok {
		return id
	}
	best := ""
	for alias, key := range b.manualNodeKeys {
		status := b.manualNodeStatuses[alias].status
		if (key == id || status.HostUUID == id || status.ClusterUUID == id) && (best == "" || alias < best) {
			best = alias
		}
	}
	if best != "" {
		return best
	}
	return id
}

// rememberManualPeer records only a current, authenticated association. The
// association helps lifecycle removal when the peer is later offline; it never
// authorizes transport or inventory.
func (b *Broker) rememberManualPeer(s manualNodeStatus) {
	if b.manualNodesConfigPath == "" || s.Kind == "engine" || s.IdentityVerification != "verified" || s.HostUUID == "" {
		return
	}
	b.manualPersistMu.Lock()
	defer b.manualPersistMu.Unlock()
	entries, err := loadPersistedManualNodes(b.manualNodesConfigPath)
	if err != nil {
		slog.Warn("failed to load manual peer association", "err", err)
		return
	}
	for i := range entries {
		if entries[i].key() == s.ID && entries[i].Address == s.Address && entries[i].Kind == "pair" && entries[i].PeerUUID == "" {
			entries[i].PeerUUID = s.HostUUID
			if err := writePersistedManualNodes(b.manualNodesConfigPath, entries); err != nil {
				slog.Warn("failed to persist manual peer association", "err", err)
			}
			return
		}
	}
}

func (b *Broker) manualAliasesForPeer(id string, addressHints ...string) ([]string, error) {
	if id == "" {
		return nil, nil
	}
	member, err := b.manualMemberForID(id)
	if err != nil {
		return nil, err
	}
	peerID := member.NodeUUID
	addresses := make(map[string]bool, len(addressHints)+1)
	if member.Address != "" {
		addresses[member.Address] = true
	}
	for _, hint := range addressHints {
		if hint != "" {
			addresses[hint] = true
		}
	}
	aliases := map[string]bool{}
	b.manualMu.Lock()
	statuses := make(map[string]manualNodeStatus, len(b.manualNodeStatuses))
	for alias, entry := range b.manualNodeStatuses {
		statuses[alias] = entry.status
	}
	b.manualMu.Unlock()
	b.manualPersistMu.Lock()
	entries, err := loadPersistedManualNodes(b.manualNodesConfigPath)
	b.manualPersistMu.Unlock()
	if err != nil {
		return nil, err
	}
	configured := make(map[string]bool, len(entries))
	for _, entry := range entries {
		alias := entry.key()
		configured[alias] = true
		if entry.Kind != "pair" {
			continue
		}
		// A current durable association wins over every cached identity,
		// reused address, or transient hint. Only unassociated entries need
		// the reachability/observation fallbacks.
		if entry.PeerUUID != "" {
			if entry.PeerUUID == peerID {
				aliases[alias] = true
			}
			continue
		}
		s := statuses[alias]
		if alias == id || addresses[entry.Address] || s.Address == entry.Address && s.Kind != "engine" && (s.HostUUID == peerID || s.ClusterUUID == peerID) {
			aliases[alias] = true
		}
	}
	// Automatically tracked cluster rows do not have durable configurations.
	for alias, s := range statuses {
		if configured[alias] || s.Kind == "engine" {
			continue
		}
		if s.HostUUID == peerID || s.ClusterUUID == peerID || alias == id || addresses[s.Address] {
			aliases[alias] = true
		}
	}
	out := make([]string, 0, len(aliases))
	for alias := range aliases {
		out = append(out, alias)
	}
	sort.Strings(out)
	return out, nil
}

// Recheck the current association atomically with the durable deletion: a
// pairing completion can reassociate an alias while the removal RPC is pending,
// after either snapshot was collected.
func (b *Broker) forgetPersistedManualPeerAlias(alias, peerID string) (bool, error) {
	b.manualPersistMu.Lock()
	defer b.manualPersistMu.Unlock()
	entries, err := loadPersistedManualNodes(b.manualNodesConfigPath)
	if err != nil {
		return false, err
	}
	out := entries[:0]
	found := false
	for _, entry := range entries {
		if entry.key() != alias {
			out = append(out, entry)
			continue
		}
		if entry.Kind != "pair" || entry.PeerUUID != "" && entry.PeerUUID != peerID {
			return false, nil
		}
		found = true
	}
	if !found {
		return true, nil
	} // an automatically tracked, non-durable row
	if err := writePersistedManualNodes(b.manualNodesConfigPath, out); err != nil {
		return false, err
	}
	return true, nil
}

// Caller serializes this transaction with manual-node mutations.
func (b *Broker) forgetManualAliases(aliases []string, peerID string) error {
	for _, alias := range aliases {
		removed, err := b.forgetPersistedManualPeerAlias(alias, peerID)
		if err != nil {
			return err
		}
		if !removed {
			continue
		}
		if worker := b.getManualNodes(); worker != nil {
			params, _ := json.Marshal(map[string]string{"id": alias})
			_, rpcErr, err := worker.Call(context.Background(), "node/remove", params)
			if err != nil {
				return err
			}
			if rpcErr != nil {
				return fmt.Errorf("forget manual entry: %s", rpcErr.Message)
			}
		}
		b.removeManualNode(alias)
	}
	return nil
}

func manualNodeTelemetry(status manualNodeStatus, hostUUID string) noderec.NodeTelemetry {
	var utilization uint32
	for i := range status.GPUs {
		if reading := status.GPUs[i].UtilizationPercent; reading != nil && *reading > utilization {
			utilization = *reading
		}
	}
	return noderec.NodeTelemetry{
		HostUUID:          hostUUID,
		PeerRecordID:      status.PeerRecordID,
		GPUUtilizationPct: utilization,
		TelemetryValid:    status.TelemetryValid,
		MSSince:           status.MSSince,
		SampleSeq:         status.SampleSeq,
	}
}

func (b *Broker) ingestManualNodeTelemetry(t manualNodeTelemetryParams) {
	b.manualMu.Lock()
	key, ok := b.manualNodeKeys[t.ID]
	b.manualMu.Unlock()
	if !ok {
		slog.Debug("dropping telemetry for unknown manual node", "id", t.ID)
		return
	}
	if t.HostUUID != "" && t.HostUUID != key {
		slog.Debug("dropping telemetry with mismatched hostUuid", "id", t.ID, "payloadHostUuid", t.HostUUID, "registeredKey", key)
		return
	}
	b.ingestTelemetryAt(sourceManual, noderec.NodeTelemetry{
		HostUUID:          key,
		PeerRecordID:      t.PeerRecordID,
		GPUUtilizationPct: t.GPUUtilizationPct,
		TelemetryValid:    t.TelemetryValid,
		MSSince:           t.MSSince,
		SampleSeq:         t.SampleSeq,
	}, time.Now())
}

// manualToEnriched projects a manual node's status onto the EnrichedNode
// the discovery store holds, so manual nodes share the single
// discovery:get-nodes / discovery:nodes-changed snapshot with mDNS nodes.
// Port is the node-info HTTP port the prober reached (mirroring the SRV
// port the scanner records for mDNS nodes); the user-entered address
// becomes the node's host/address so the snapshot's ipAddress projection
// can resolve it when the address is an IP literal.
func manualToEnriched(s manualNodeStatus) EnrichedNode {
	// Every node carries a non-empty operational key by the time it reaches the
	// store: the remote's real hostUuid once node-info reports it, else the
	// manual id (the user's name or "manual:<address>") until then. This is the
	// manual-node ingestion boundary — downstream keys off HostUUID with no
	// fallback. Once the real UUID is learned, a manually-added machine
	// that's also discovered over mDNS collapses to the one hostUuid-keyed entry.
	hostUUID := s.HostUUID
	if hostUUID == "" || s.PeerRecordID == "" {
		hostUUID = s.ID
	}
	recordID := s.PeerRecordID
	verification := "verified"
	if recordID == "" {
		recordID = noderec.PreviewRecordID(s.ID, "")
		verification = "unverified"
		if s.Kind == "engine" && s.IdentityVerification == "manual" {
			verification = "manual"
			recordID = noderec.ManualRecordID(s.ID)
		}
	}
	models := s.Models
	if len(models) == 0 {
		models = mergeModels(s.OllamaModels, s.LMStudioModels, s.OMLXModels, s.LlamaCPPModels)
	}
	modelsByEngine := s.ModelsByEngine
	if len(modelsByEngine) == 0 {
		modelsByEngine = manualModelsByEngine(s)
	}
	en := EnrichedNode{
		ID:                     s.ID,
		HostUUID:               hostUUID,
		Host:                   s.Address,
		Port:                   s.NodeInfoPort,
		GPUs:                   s.GPUs,
		CPU:                    s.CPU,
		Memory:                 s.Memory,
		Models:                 models,
		ModelsByEngine:         modelsByEngine,
		LoadedByEngine:         s.LoadedByEngine,
		DownloadedByEngine:     s.DownloadedByEngine,
		DownloadErrorsByEngine: s.DownloadErrorsByEngine,
		EngineNative:           s.EngineNative,
		Clustered:              s.ClusterUUID != "",
		PeerRecordID:           recordID,
		IdentityVerification:   verification,
		ClusterUUID:            s.ClusterUUID,
		ManualEngine:           verification == "manual",
	}
	if s.Address != "" {
		en.Addresses = []string{s.Address}
	}
	txt := []string{
		noderec.KeySchema + "=" + noderec.SchemaVersion,
		noderec.KeyHostUUID + "=" + hostUUID,
	}
	if s.Address != "" {
		txt = append(txt, noderec.KeyIP+"="+s.Address)
	}
	if s.ClusterUUID != "" {
		txt = append(txt, noderec.KeyClusterUUID+"="+s.ClusterUUID)
	}
	en.TXT = txt
	return en
}

func (b *Broker) annotateManualTrust(en *EnrichedNode, s manualNodeStatus) {
	if b.mesh == nil {
		return
	}
	b.mesh.Refresh()
	en.Trusted = s.HostUUID == s.ClusterUUID && s.PeerMetadata != nil && s.PeerMetadata.HostUUID == s.HostUUID && b.currentPeerProof(s.ClusterUUID, s.PeerRecordID)
	if !en.Trusted && en.IdentityVerification == "verified" {
		en.IdentityVerification = "unverified"
		en.PeerRecordID = noderec.PreviewRecordID(s.ID, "")
	}
	if en.Trusted {
		en.Clustered = true
		en.IdentityVerification = "verified"
	}
}

func (b *Broker) manualToDirectoryNode(s manualNodeStatus, key string) noderec.DirectoryNode {
	if s.PeerRecordID != "" && s.PeerMetadata != nil && s.HostUUID == s.ClusterUUID && b.currentPeerProof(s.ClusterUUID, s.PeerRecordID) {
		services := make(map[noderec.ServiceKey]noderec.ServiceStatus, len(s.PeerMetadata.Services))
		for service, port := range s.PeerMetadata.Services {
			services[service] = noderec.ServiceStatus{Port: port}
		}
		return noderec.DirectoryNode{HostUUID: s.HostUUID, Name: s.PeerMetadata.Name, IP: s.Address, IPs: []string{s.Address}, ClusterUUID: s.ClusterUUID, Trusted: true, IdentityVerification: "verified", PeerRecordID: s.PeerRecordID, Services: services, GPUs: s.GPUs, CPU: s.CPU, Memory: s.Memory, Models: s.Models, ModelsByEngine: s.ModelsByEngine, LoadedByEngine: s.LoadedByEngine, DownloadedByEngine: s.DownloadedByEngine, DownloadErrorsByEngine: s.DownloadErrorsByEngine, EngineNative: s.EngineNative, LastSeen: time.Now().Unix()}
	}
	hostUUID := s.HostUUID
	if hostUUID == "" {
		hostUUID = key
	}
	trusted := false
	clusterUUID := s.ClusterUUID
	// Public/manual IDs never become peer proofs merely because a pin exists.
	services := make(map[noderec.ServiceKey]noderec.ServiceStatus)
	if s.NodeInfoUp {
		port := s.NodeInfoPort
		if port == 0 {
			port = 14318
		}
		services[noderec.ServiceNodeInfo] = noderec.ServiceStatus{Port: port}
		services[noderec.ServiceEngineManager] = noderec.ServiceStatus{Port: 14322}
		services[noderec.ServiceEngineControl] = noderec.ServiceStatus{Port: 14323}
		services[noderec.ServiceErrors] = noderec.ServiceStatus{Port: 14319}
		services[noderec.ServiceWorkload] = noderec.ServiceStatus{Port: 14320}
		services[noderec.ServiceCluster] = noderec.ServiceStatus{Port: 14321}
	}
	modelsByEngine := s.ModelsByEngine
	if len(modelsByEngine) == 0 {
		modelsByEngine = manualModelsByEngine(s)
	}
	ollamaUp := s.OllamaUp
	if !ollamaUp && modelsByEngine != nil {
		if _, ok := modelsByEngine["ollama"]; ok {
			ollamaUp = true
		}
	}
	if ollamaUp {
		port := s.OllamaPort
		if port == 0 {
			port = 11434
		}
		services[noderec.ServiceOllama] = noderec.ServiceStatus{Port: port}
	}
	lmStudioUp := s.LMStudioUp
	if !lmStudioUp && modelsByEngine != nil {
		if _, ok := modelsByEngine["lmstudio"]; ok {
			lmStudioUp = true
		}
	}
	if lmStudioUp {
		port := s.LMStudioPort
		if port == 0 {
			port = 1234
		}
		services[noderec.ServiceLMStudio] = noderec.ServiceStatus{Port: port}
	}
	omlxUp := s.OMLXUp
	if !omlxUp && modelsByEngine != nil {
		if _, ok := modelsByEngine["omlx"]; ok {
			omlxUp = true
		}
	}
	if omlxUp {
		port := s.OMLXPort
		if port == 0 {
			port = 1236
		}
		services[noderec.ServiceOMLX] = noderec.ServiceStatus{Port: port}
	}
	llamacppUp := s.LlamaCPPUp
	if !llamacppUp && modelsByEngine != nil {
		if _, ok := modelsByEngine["llamacpp"]; ok {
			llamacppUp = true
		}
	}
	if llamacppUp {
		port := s.LlamaCPPPort
		if port == 0 {
			port = 8082
		}
		services[noderec.ServiceLlamaCPP] = noderec.ServiceStatus{Port: port}
	}

	ips := []string{}
	if s.Address != "" {
		ips = []string{s.Address}
	}

	models := s.Models
	if len(models) == 0 {
		models = mergeModels(s.OllamaModels, s.LMStudioModels, s.OMLXModels, s.LlamaCPPModels)
	}

	verification := "unverified"
	recordID := noderec.PreviewRecordID(s.ID, "")
	if s.Kind == "engine" && s.IdentityVerification == "manual" {
		verification = "manual"
		recordID = noderec.ManualRecordID(s.ID)
	}
	return noderec.DirectoryNode{
		PeerRecordID:           recordID,
		IdentityVerification:   verification,
		HostUUID:               hostUUID,
		Name:                   s.ID,
		IP:                     s.Address,
		IPs:                    ips,
		ClusterUUID:            clusterUUID,
		Trusted:                trusted,
		Services:               services,
		GPUs:                   s.GPUs,
		CPU:                    s.CPU,
		Memory:                 s.Memory,
		Models:                 models,
		ModelsByEngine:         modelsByEngine,
		LoadedByEngine:         s.LoadedByEngine,
		DownloadedByEngine:     s.DownloadedByEngine,
		DownloadErrorsByEngine: s.DownloadErrorsByEngine,
		EngineNative:           s.EngineNative,
		LastSeen:               time.Now().Unix(),
	}
}

func (b *Broker) refreshManualNodesTrust() {
	// Snapshot and replay are one operation relative to worker callbacks and
	// retirement. Never let a copied status outlive its worker generation.
	b.manualCallbackMu.Lock()
	defer b.manualCallbackMu.Unlock()
	generation := b.manualInventoryGeneration.Load()
	b.manualMu.Lock()
	seenKeys := make(map[string]struct{}, len(b.manualNodeKeys))
	entries := make([]manualNodeStatusEntry, 0, len(b.manualNodeKeys))
	for _, key := range b.manualNodeKeys {
		if key == "" {
			continue
		}
		if _, dup := seenKeys[key]; dup {
			continue
		}
		seenKeys[key] = struct{}{}
		if canonical, ok := b.survivingAliasLocked(key); ok && canonical.generation == generation {
			entries = append(entries, canonical)
		}
	}
	b.manualMu.Unlock()

	for _, entry := range entries {
		b.upsertManualNode(entry.status)
	}
}

// manualModelsByEngine builds the per-engine attribution for a manual node from
// the per-engine lists the prober already collected, keyed by the same
// engine-manager engine names discovered nodes use ("ollama", "lmstudio") so the
// two discovery sources present ModelsByEngine identically. An engine with no
// models adds no key; returns nil when neither engine reports any.
func manualModelsByEngine(s manualNodeStatus) map[string][]string {
	byEngine := map[string][]string{}
	if len(s.OllamaModels) > 0 {
		byEngine["ollama"] = s.OllamaModels
	}
	if len(s.LMStudioModels) > 0 {
		byEngine["lmstudio"] = s.LMStudioModels
	}
	if len(s.OMLXModels) > 0 {
		byEngine["omlx"] = s.OMLXModels
	}
	if len(s.LlamaCPPModels) > 0 {
		byEngine["llamacpp"] = s.LlamaCPPModels
	}
	if len(byEngine) == 0 {
		return nil
	}
	return byEngine
}

// mergeModels unions per-engine model lists into one de-duplicated,
// order-preserving slice, so a manual node surfaces its models on
// AvailableNode.models the same way a discovered node does. Manual nodes are
// probed directly by nvpair-manual-nodes (models arrive on the status), rather than
// enriched over engine-manager's em HTTP endpoint like discovered nodes.
func mergeModels(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, m := range l {
			if m != "" && !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

// proxyManualNode is the node/add-manual payload the broker hands
// ollama-proxy to bridge a reachable manual node into inference routing.
// It mirrors the proxy's Node wire shape (id/host/port/addresses[/txt]);
// the proxy requires a non-empty address list and a port to forward to.
type proxyManualNode struct {
	ID        string   `json:"id"`
	Host      string   `json:"host"`
	Port      int      `json:"port"`
	Addresses []string `json:"addresses"`
	TXT       []string `json:"txt,omitempty"`
	Models    []string `json:"models,omitempty"`
}

// bridgeManualNode keeps every supervised proxy's manual-node set in step with
// a manual node's per-engine reachability: a node whose Ollama is up is bridged
// into ollama-proxy and one whose LM Studio is up into lmstudio-proxy
// (idempotent — each proxy upserts on a repeat), while an engine that is not
// (or no longer) reachable is removed from its proxy. Each leg is a no-op when
// that proxy isn't supervised — the bridge only applies when the broker owns
// both ends.
//
// Manual nodes are, by definition, the nodes that never appear in the discovery
// relay's snapshots — they advertise no _nvpair-node record for the scanner
// daemon to carry — so without this explicit add the proxies can't route
// inference to them even though both workers are broker-owned.
func (b *Broker) bridgeManualNode(s manualNodeStatus, key string) {
	ollamaModels := s.OllamaModels
	if len(ollamaModels) == 0 && s.ModelsByEngine != nil {
		ollamaModels = s.ModelsByEngine["ollama"]
	}
	ollamaUp := s.OllamaUp || len(ollamaModels) > 0
	ollamaPort := s.OllamaPort
	if ollamaPort == 0 {
		ollamaPort = 11434
	}

	lmStudioModels := s.LMStudioModels
	if len(lmStudioModels) == 0 && s.ModelsByEngine != nil {
		lmStudioModels = s.ModelsByEngine["lmstudio"]
	}
	lmStudioUp := s.LMStudioUp || len(lmStudioModels) > 0
	lmStudioPort := s.LMStudioPort
	if lmStudioPort == 0 {
		lmStudioPort = 1234
	}

	b.bridgeToProxy(b.getProxy(), "ollama", s, key, ollamaUp, ollamaPort, ollamaModels)
	b.bridgeToProxy(b.getLMStudioProxy(), "lmstudio", s, key, lmStudioUp, lmStudioPort, lmStudioModels)

	var omlxModels []string
	if len(s.OMLXModels) > 0 {
		omlxModels = s.OMLXModels
	} else if s.ModelsByEngine != nil {
		omlxModels = s.ModelsByEngine["omlx"]
	}
	omlxUp := s.OMLXUp || len(omlxModels) > 0
	omlxPort := s.OMLXPort
	if omlxPort == 0 {
		omlxPort = 1236
	}
	b.bridgeToProxy(b.getOMLXProxy(), "omlx", s, key, omlxUp, omlxPort, omlxModels)
	llamacppModels := s.LlamaCPPModels
	if len(llamacppModels) == 0 && s.ModelsByEngine != nil {
		llamacppModels = s.ModelsByEngine["llamacpp"]
	}
	llamacppPort := s.LlamaCPPPort
	if llamacppPort == 0 {
		llamacppPort = 8082
	}
	b.bridgeToProxy(b.getLlamaCPPProxy(), "llamacpp", s, key, s.LlamaCPPUp || len(llamacppModels) > 0, llamacppPort, llamacppModels)
}

// bridgeToProxy adds the node to p when its engine is reachable, or removes it
// otherwise. up/port/models are the engine-specific fields the caller pulled
// off the node's status. The proxy candidate is keyed by `key` — the node's
// operational identity (its hostUuid once node-info reports it, else the manual
// id) — the same key the discovery store and scheduler use, so the scheduler's
// priority list and scheduledOn resolve to this candidate.
func (b *Broker) bridgeToProxy(p *proxyProcess, engine string, s manualNodeStatus, key string, up bool, port int, models []string) {
	if p == nil {
		return
	}
	// Proven PAIR peers travel exclusively through the authenticated relay.
	// Only independent, explicitly configured bare engines use this bypass.
	if up && s.Kind == "engine" && s.IdentityVerification == "manual" && s.HostUUID == "" && s.ClusterUUID == "" && s.PeerRecordID == "" && s.Address != "" && port > 0 {
		node := proxyManualNode{
			ID:        key,
			Host:      s.Address,
			Port:      port,
			Addresses: []string{s.Address},
			Models:    models,
		}
		b.callProxyManual(p, engine, "node/add-manual", node, key)
		return
	}
	// Engine unreachable (down, or this node doesn't run it): make sure the
	// proxy isn't left holding a stale manual entry it would try to route to.
	b.callProxyManual(p, engine, "node/remove-manual", map[string]string{"id": key}, key)
}

// removeManualNodeFromProxies drops a manual node from every supervised proxy.
// Idempotent: a no-op for a proxy where the node was never bridged or that
// isn't supervised (the proxy's RemoveManual just reports removed=false).
func (b *Broker) removeManualNodeFromProxies(id string) {
	b.callProxyManual(b.getProxy(), "ollama", "node/remove-manual", map[string]string{"id": id}, id)
	b.callProxyManual(b.getLMStudioProxy(), "lmstudio", "node/remove-manual", map[string]string{"id": id}, id)
	b.callProxyManual(b.getOMLXProxy(), "omlx", "node/remove-manual", map[string]string{"id": id}, id)
	b.callProxyManual(b.getLlamaCPPProxy(), "llamacpp", "node/remove-manual", map[string]string{"id": id}, id)
}

// callProxyManual issues a best-effort node/add-manual|remove-manual to a
// proxy. The bridge is advisory plumbing the broker owns, not part of the
// manual-node request's own response, so a failure is logged and swallowed
// rather than surfaced to the client. A nil proxy (not supervised) is a no-op.
// Runs synchronously on the manual-nodes reader goroutine; a proxy answers
// these control-plane calls locally in well under proxyCallTimeout, and
// manual-node events are infrequent (one per 10s probe), so the brief inline
// call keeps add/remove strictly ordered without a queue.
func (b *Broker) callProxyManual(p *proxyProcess, engine, method string, params any, id string) {
	if p == nil {
		return
	}
	raw, err := json.Marshal(params)
	if err != nil {
		slog.Warn("manual->proxy bridge: marshal failed", "engine", engine, "method", method, "id", id, "err", err)
		return
	}
	if _, rpcErr, err := p.Call(context.Background(), method, raw); err != nil {
		slog.Warn("manual->proxy bridge failed", "engine", engine, "method", method, "id", id, "err", err)
	} else if rpcErr != nil {
		slog.Warn("manual->proxy bridge rejected", "engine", engine, "method", method, "id", id, "code", rpcErr.Code, "msg", rpcErr.Message)
	}
}
