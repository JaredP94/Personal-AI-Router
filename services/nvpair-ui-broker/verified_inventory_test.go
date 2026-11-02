// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/noderec"
	"nvpair-ui-broker/relay"
	"nvpair-ui-broker/workloadstore"
	"testing"
	"time"
)

func inventoryTestBroker(t *testing.T) *Broker {
	t.Helper()
	dir := t.TempDir()
	clustertrusttest.Join(t, dir, "test-cluster", "self", "peer")
	b := &Broker{nodeID: "self", clusterDir: dir, mesh: clustertrust.Open(dir), store: newDiscoveryStore(), relayDir: relay.NewDirectory(), telemetry: newTelemetryCache(), workloads: workloadstore.New(), manualNodeKeys: make(map[string]string), manualNodeStatuses: make(map[string]manualNodeStatusEntry)}
	b.relayDir.Authorize = b.authorizedDirectoryNode
	return b
}
func verifiedDirectoryForTest(b *Broker) noderec.DirectoryNode {
	return noderec.DirectoryNode{HostUUID: "peer", ClusterUUID: "peer", Name: "peer", IP: "192.0.2.2", Trusted: true, IdentityVerification: "verified", PeerRecordID: b.mesh.PeerRecordID("peer"), Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceNodeInfo: {Port: 14318}, noderec.ServiceOllama: {Port: 11434}, noderec.ServiceLMStudio: {Port: 1234}, noderec.ServiceOMLX: {Port: 1236}, noderec.ServiceEngineControl: {Port: 14323}}, Models: []string{"model"}}
}

func TestClaimedManualPrincipalIsNotTrustedOrScheduled(t *testing.T) {
	b := inventoryTestBroker(t)
	s := manualNodeStatus{ID: "manual:preview", Address: "192.0.2.2", HostUUID: "peer", ClusterUUID: "peer", NodeInfoUp: true, OllamaUp: true, OllamaModels: []string{"forged"}, TelemetryValid: true}
	en := manualToEnriched(s)
	b.annotateManualTrust(&en, s)
	if en.Trusted {
		t.Fatal("a reported pinned ID granted trust without an mTLS proof")
	}
	b.upsertManualNode(s)
	if got := b.schedulerNodes(); len(got) != 0 {
		t.Fatalf("unproven inventory in scheduler: %+v", got)
	}
	for _, service := range []noderec.ServiceKey{noderec.ServiceOllama, noderec.ServiceLMStudio, noderec.ServiceOMLX, noderec.ServiceEngineControl} {
		if got := b.relayDir.Snapshot(service); len(got) != 0 {
			t.Fatalf("unproven %s target: %+v", service, got)
		}
	}
	b.ingestTelemetry(sourceManual, noderec.NodeTelemetry{HostUUID: "peer", TelemetryValid: true, GPUUtilizationPct: 99})
	if len(b.telemetry.Snapshot(time.Now())) != 0 {
		t.Fatal("unproven telemetry entered scheduler cache")
	}
}

// Invalid readings still affect scheduler state: they must carry the same
// provenance as valid samples, rather than clearing a proven peer's pressure.
func TestUnprovenInvalidTelemetryCannotReplaceAuthenticatedSample(t *testing.T) {
	for name, source := range map[string]nodeSource{"scanner": sourceScanner, "manual": sourceManual} {
		t.Run(name, func(t *testing.T) {
			b := inventoryTestBroker(t)
			node := verifiedDirectoryForTest(b)
			b.store.Upsert(directoryToEnriched(node), source)
			b.ingestTelemetry(source, noderec.NodeTelemetry{HostUUID: "peer", PeerRecordID: node.PeerRecordID, TelemetryValid: true, GPUUtilizationPct: 73, SampleSeq: 8})
			b.ingestTelemetry(source, noderec.NodeTelemetry{HostUUID: "peer", TelemetryValid: false, GPUUtilizationPct: 99, SampleSeq: 900})
			got := b.telemetry.Snapshot(time.Now())
			if len(got) != 1 || !got[0].TelemetryValid || got[0].GPUUtilizationPct != 73 || got[0].SampleSeq != 8 {
				t.Fatalf("unproven invalid sample replaced authenticated pressure: %+v", got)
			}
			// A current peer may legitimately report an unavailable sample.
			b.ingestTelemetry(source, noderec.NodeTelemetry{HostUUID: "peer", PeerRecordID: node.PeerRecordID, TelemetryValid: false, SampleSeq: 9})
			got = b.telemetry.Snapshot(time.Now())
			if len(got) != 1 || got[0].TelemetryValid || got[0].SampleSeq != 9 {
				t.Fatalf("authenticated invalid sample rejected: %+v", got)
			}
		})
	}
}

func TestReplayInvalidTelemetryRequiresCurrentProof(t *testing.T) {
	b := inventoryTestBroker(t)
	node := verifiedDirectoryForTest(b)
	b.store.Upsert(directoryToEnriched(node), sourceScanner)
	b.ingestTelemetry(sourceScanner, noderec.NodeTelemetry{HostUUID: "peer", PeerRecordID: node.PeerRecordID, SampleSeq: 19})
	var out bytes.Buffer
	worker := &rpcWorker{peer: NewPeer(NewCodec(&out))}
	if got := b.replayTelemetryToScheduler(worker); got != 1 || out.Len() == 0 {
		t.Fatalf("current peer's unavailable sample was not replayed: count=%d", got)
	}
	out.Reset()
	clustertrusttest.RemovePeerPin(t, b.clusterDir, "peer")
	if got := b.replayTelemetryToScheduler(worker); got != 0 || out.Len() != 0 {
		t.Fatalf("retired invalid sample reached scheduler: count=%d frame=%s", got, out.String())
	}
}

func TestVerifiedInventoryWithdrawnOnPinAndAdmissionChanges(t *testing.T) {
	for _, change := range []string{"pin", "admission"} {
		t.Run(change, func(t *testing.T) {
			b := inventoryTestBroker(t)
			node := verifiedDirectoryForTest(b)
			b.store.Upsert(directoryToEnriched(node), sourceScanner)
			b.relayDir.Apply(noderec.NotifyNodeDiscovered, node)
			b.ingestTelemetry(sourceScanner, noderec.NodeTelemetry{HostUUID: "peer", PeerRecordID: node.PeerRecordID, TelemetryValid: true, GPUUtilizationPct: 17})
			if len(b.schedulerNodes()) != 1 || len(b.relayDir.Snapshot(noderec.ServiceOMLX)) != 1 || len(b.telemetry.Snapshot(time.Now())) != 1 {
				t.Fatal("current proof not accepted")
			}
			if change == "pin" {
				clustertrusttest.RemovePeerPin(t, b.clusterDir, "peer")
			} else {
				clustertrusttest.WriteAdmission(t, b.clusterDir, "test-cluster", 2)
			}
			// Cached relay/scheduler reads independently recheck authorization, before
			// the worker has reacted to the notification.
			if len(b.schedulerNodes()) != 0 || len(b.relayDir.Snapshot(noderec.ServiceOllama)) != 0 {
				t.Fatal("stale proof remained service eligible")
			}
			b.withdrawUnauthorizedInventory()
			b.ingestTelemetry(sourceScanner, noderec.NodeTelemetry{HostUUID: "peer", PeerRecordID: node.PeerRecordID, TelemetryValid: true, GPUUtilizationPct: 99})
			for _, sample := range b.telemetry.Snapshot(time.Now()) {
				if sample.TelemetryValid {
					t.Fatal("retired telemetry revived")
				}
			}
		})
	}
}

func TestUnprovenScannerClaimCannotReplaceVerifiedPeer(t *testing.T) {
	b := inventoryTestBroker(t)
	node := verifiedDirectoryForTest(b)
	sp := &scannerProcess{store: b.store, relayDir: b.relayDir, authorize: b.authorizedDirectoryNode, onTelemetryRemoved: func(host string) { b.removeTelemetry(sourceScanner, host) }}
	send := func(method string, n noderec.DirectoryNode) {
		data, _ := json.Marshal(noderec.NodeEvent{Node: n})
		sp.handleNotify(method, data)
	}
	send(noderec.NotifyNodeDiscovered, node)
	forged := node
	forged.PeerRecordID = noderec.PreviewRecordID(forged.Name, forged.HostUUID)
	forged.IdentityVerification = "unverified"
	forged.Trusted = false
	forged.Models = []string{"forged"}
	b.ingestTelemetry(sourceScanner, noderec.NodeTelemetry{HostUUID: "peer", PeerRecordID: node.PeerRecordID, TelemetryValid: true})
	send(noderec.NotifyNodeUpdated, forged)
	if len(b.store.Snapshot()) != 2 {
		t.Fatal("preview did not get an independent record key")
	}
	send(noderec.NotifyNodeRemoved, forged)
	if len(b.store.Snapshot()) != 1 || len(b.telemetry.Snapshot(time.Now())) != 1 {
		t.Fatal("preview removal affected verified source state")
	}
	got := b.schedulerNodes()
	if len(got) != 1 || len(got[0].Models) != 1 || got[0].Models[0] != "model" {
		t.Fatalf("public claim displaced verified peer: %+v", got)
	}
}

func TestScannerRestartWithdrawsProofButPreservesIndependentManualEngine(t *testing.T) {
	b := inventoryTestBroker(t)
	node := verifiedDirectoryForTest(b)
	b.store.Upsert(directoryToEnriched(node), sourceScanner)
	b.relayDir.Apply(noderec.NotifyNodeDiscovered, node)
	b.upsertManualNode(manualNodeStatus{ID: "manual:engine", Address: "192.0.2.3", OMLXUp: true, OMLXPort: 1236, OMLXModels: []string{"bare-model"}})
	b.clearScannerInventory()
	got := b.schedulerNodes()
	if len(got) != 1 || got[0].HostUUID != noderec.ManualRecordID("manual:engine") || len(got[0].Models) != 1 || got[0].Models[0] != "bare-model" {
		t.Fatalf("restart sources = %+v", got)
	}
}
