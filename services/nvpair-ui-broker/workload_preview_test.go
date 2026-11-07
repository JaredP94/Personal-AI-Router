// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"nvpair-shared/clustertrusttest"
	"nvpair-shared/noderec"
	"nvpair-ui-broker/workloadstore"
)

// Rotating the pin before discovery publishes its renewed proof must not
// discard the local proxy's authoritative report about an already routed job.
func TestLocalWorkloadNotLostDuringPeerCertificateRenewal(t *testing.T) {
	for _, engine := range []string{"ollama", "lmstudio", "omlx", "llamacpp"} {
		for _, departure := range []string{"renewed-then-lost", "trust-withdrawal", "scanner-retirement"} {
			t.Run(engine+"/"+departure, func(t *testing.T) {
				b := inventoryTestBroker(t)
				var client, scheduler bytes.Buffer
				b.codec = NewCodec(&client)
				b.workloadsSubscribed = true
				b.setScheduler(&rpcWorker{peer: NewPeer(NewCodec(&scheduler))})
				node := verifiedDirectoryForTest(b)
				b.store.Upsert(directoryToEnriched(node), sourceScanner)
				if !b.hasDecisionInventory("peer") {
					t.Fatal("initial peer proof was not accepted")
				}
				clustertrusttest.WritePeerPin(t, b.clusterDir, "peer")
				b.mesh.Refresh()
				if b.hasDecisionInventory("peer") {
					t.Fatal("old proof remained current after certificate renewal")
				}

				for _, event := range []struct{ method, state string }{{"workload:submitted", "queued"}, {"workload:started", "running"}} {
					params, err := json.Marshal(map[string]any{"workloadInfo": map[string]any{
						"id": "7", "engine": engine, "runId": "r1", "state": event.state,
						"scheduledOn": "peer", "createdAt": 1, "model": "m",
					}})
					if err != nil {
						t.Fatal(err)
					}
					b.routeProxyWorkloadMethod(event.method, params)
					got, ok := b.workloads.Get("self", "7")
					if !ok || got.State != event.state || got.Origin != "self" || got.ScheduledOn != "peer" {
						t.Fatalf("local %s lost or rekeyed during renewal: %+v", event.method, got)
					}
				}

				switch departure {
				case "renewed-then-lost":
					node = verifiedDirectoryForTest(b)
					b.store.Upsert(directoryToEnriched(node), sourceScanner)
					if !b.hasDecisionInventory("peer") {
						t.Fatal("renewed proof was not accepted")
					}
					sp := &scannerProcess{store: b.store, authorize: b.authorizedDirectoryNode, onNodeLost: func(uuid, name string) {
						if !b.hasDecisionInventory(uuid) {
							b.failWorkloadsForNode(uuid, name)
						}
					}}
					params, err := json.Marshal(noderec.NodeEvent{Node: node})
					if err != nil {
						t.Fatal(err)
					}
					sp.handleNotify(noderec.NotifyNodeRemoved, params)
				case "trust-withdrawal":
					clustertrusttest.RemovePeerPin(t, b.clusterDir, "peer")
					b.withdrawUnauthorizedInventory()
				case "scanner-retirement":
					b.clearScannerInventory()
				}
				if got, ok := b.workloads.Get("self", "7"); !ok || got.State != "failed" {
					t.Fatalf("local workload after %s = %+v, want failed", departure, got)
				}
				for name, output := range map[string]*bytes.Buffer{"client": &client, "scheduler": &scheduler} {
					dec := json.NewDecoder(output)
					for _, state := range []string{"queued", "running", "failed"} {
						var msg Message
						if err := dec.Decode(&msg); err != nil {
							t.Fatalf("%s missing %s notification: %v", name, state, err)
						}
						var env struct {
							WorkloadInfo json.RawMessage `json:"workloadInfo"`
						}
						if err := json.Unmarshal(msg.Params, &env); err != nil {
							t.Fatal(err)
						}
						got, ok := workloadstore.ParseIncoming(env.WorkloadInfo)
						if msg.Method != "workloads:upsert" || !ok || got.State != state || got.Origin != "self" || got.ScheduledOn != "peer" {
							t.Fatalf("%s notification = %s %+v, want %s with stable participants", name, msg.Method, got, state)
						}
					}
					if dec.More() {
						t.Fatalf("%s received extra workload notifications", name)
					}
				}
			})
		}
	}
}

// A relayed claim cannot opt into local-proxy authority by naming our node as
// its origin, even when it describes the same job a local proxy could report.
func TestRemoteWorkloadClaimRejectedAgainstUnprovenInventory(t *testing.T) {
	for _, inventory := range []string{"preview", "renewing"} {
		t.Run(inventory, func(t *testing.T) {
			b := inventoryTestBroker(t)
			if inventory == "preview" {
				b.store.Upsert(EnrichedNode{HostUUID: "peer", ID: "unproven", IdentityVerification: "unverified", PeerRecordID: noderec.PreviewRecordID("unproven", "peer")}, sourceScanner)
			} else {
				b.store.Upsert(directoryToEnriched(verifiedDirectoryForTest(b)), sourceScanner)
				clustertrusttest.WritePeerPin(t, b.clusterDir, "peer")
				b.mesh.Refresh()
			}
			var client, scheduler bytes.Buffer
			b.codec = NewCodec(&client)
			b.workloadsSubscribed = true
			b.setScheduler(&rpcWorker{peer: NewPeer(NewCodec(&scheduler))})
			incoming := storeIncoming("7", "self", "ollama", "r1", "running", "peer")
			params, err := json.Marshal(map[string]json.RawMessage{"workloadInfo": incoming.Info})
			if err != nil {
				t.Fatal(err)
			}
			b.forwardWorkloadManagerNotification("workloads:upsert", params)
			if _, ok := b.workloads.Get("self", "7"); ok || client.Len() != 0 || scheduler.Len() != 0 {
				t.Fatal("unverified remote claim reached the catalog, client or scheduler")
			}
		})
	}
}

func TestLocalProxyWorkloadOnPreviewFailsOnWithdrawal(t *testing.T) {
	b := inventoryTestBroker(t)
	preview := noderec.DirectoryNode{HostUUID: "peer", Name: "unproven", IdentityVerification: "unverified", PeerRecordID: noderec.PreviewRecordID("unproven", "peer")}
	b.store.Upsert(directoryToEnriched(preview), sourceScanner)
	incoming := storeIncoming("7", "self", "omlx", "r1", "running", "peer")
	params, err := json.Marshal(map[string]json.RawMessage{"workloadInfo": incoming.Info})
	if err != nil {
		t.Fatal(err)
	}
	b.routeProxyWorkloadMethod("workload:started", params)
	if got, ok := b.workloads.Get("self", "7"); !ok || got.State != "running" || got.ScheduledOn != "peer" {
		t.Fatalf("local proxy report was not tracked by stable participant: %+v", got)
	}
	sp := &scannerProcess{store: b.store, authorize: b.authorizedDirectoryNode, onNodeLost: func(uuid, name string) {
		if !b.hasDecisionInventory(uuid) {
			b.failWorkloadsForNode(uuid, name)
		}
	}}
	params, err = json.Marshal(noderec.NodeEvent{Node: preview})
	if err != nil {
		t.Fatal(err)
	}
	sp.handleNotify(noderec.NotifyNodeRemoved, params)
	if got, ok := b.workloads.Get("self", "7"); !ok || got.State != "failed" {
		t.Fatalf("local proxy workload after preview loss = %+v, want failed", got)
	}
}

func TestPreviewWithdrawalRetiresAlreadyTrackedWorkloads(t *testing.T) {
	for _, verifiedSurvives := range []bool{false, true} {
		t.Run(map[bool]string{false: "preview-only", true: "verified-survives"}[verifiedSurvives], func(t *testing.T) {
			b := inventoryTestBroker(t)
			preview := noderec.DirectoryNode{HostUUID: "peer", Name: "unproven", IdentityVerification: "unverified", PeerRecordID: noderec.PreviewRecordID("unproven", "peer")}
			b.store.Upsert(directoryToEnriched(preview), sourceScanner)
			if verifiedSurvives {
				b.store.Upsert(directoryToEnriched(verifiedDirectoryForTest(b)), sourceManual)
			}
			// Seed a previously accepted record: the withdrawal must clean up
			// even state admitted before preview admission was guarded.
			b.workloads.Apply(storeIncoming("7", "peer", "ollama", "r1", "running", "peer"))
			sp := &scannerProcess{store: b.store, authorize: b.authorizedDirectoryNode, onNodeLost: func(uuid, name string) {
				if !b.hasDecisionInventory(uuid) {
					b.failWorkloadsForNode(uuid, name)
				}
			}}
			params, err := json.Marshal(noderec.NodeEvent{Node: preview})
			if err != nil {
				t.Fatal(err)
			}
			sp.handleNotify(noderec.NotifyNodeRemoved, params)
			want := "failed"
			if verifiedSurvives {
				want = "running"
			}
			if got, ok := b.workloads.Get("peer", "7"); !ok || got.State != want {
				t.Fatalf("workload after preview withdrawal = %+v, want %s", got, want)
			}
		})
	}
}

func TestScannerRetirementRetiresPreviewWorkloads(t *testing.T) {
	b := inventoryTestBroker(t)
	b.store.Upsert(EnrichedNode{HostUUID: "peer", ID: "preview", IdentityVerification: "unverified", PeerRecordID: noderec.PreviewRecordID("preview", "peer")}, sourceScanner)
	b.workloads.Apply(storeIncoming("7", "peer", "ollama", "r1", "running", "peer"))
	b.clearScannerInventory()
	if got, ok := b.workloads.Get("peer", "7"); !ok || got.State != "failed" {
		t.Fatalf("workload after scanner retirement = %+v, want failed", got)
	}
}

func TestActiveWorkloadsCannotUseUnverifiedPreview(t *testing.T) {
	for _, field := range []string{"origin", "executor", "preview-key"} {
		t.Run(field, func(t *testing.T) {
			b := inventoryTestBroker(t)
			preview := noderec.DirectoryNode{HostUUID: "peer", Name: "unproven", IdentityVerification: "unverified", PeerRecordID: noderec.PreviewRecordID("unproven", "peer")}
			b.store.Upsert(directoryToEnriched(preview), sourceScanner)
			origin, executor := "self", "peer"
			if field == "origin" {
				origin, executor = "peer", "self"
			} else if field == "preview-key" {
				executor = preview.RecordKey()
			}
			incoming := storeIncoming("7", origin, "ollama", "r1", "running", executor)
			params, err := json.Marshal(map[string]json.RawMessage{"workloadInfo": incoming.Info})
			if err != nil {
				t.Fatal(err)
			}
			b.forwardWorkloadManagerNotification("workloads:upsert", params)
			if _, ok := b.workloads.Get(origin, "7"); ok {
				t.Fatal("active workload was tracked against an unverified preview")
			}
			// A proven record sharing the claimed UUID must not be shadowed
			// by an unrelated preview. The preview key itself stays forbidden.
			b.store.Upsert(directoryToEnriched(verifiedDirectoryForTest(b)), sourceManual)
			b.forwardWorkloadManagerNotification("workloads:upsert", params)
			_, accepted := b.workloads.Get(origin, "7")
			if accepted != (field != "preview-key") {
				t.Fatalf("verified admission = %v for %s", accepted, field)
			}
		})
	}
}
